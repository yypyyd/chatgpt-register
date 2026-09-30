package codexreg

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Session Observer(so) 快照解码。
//
// 官方 SDK 的 sessionObserverToken 输出：
//
//	so = base64( XOR( JSON{ regId: base64( XOR(value, K) ) ... }, K ) )
//
// K 是形如 "85.87" 的短 key（Math.random()*100 的两位小数）。快照里依次是：
// 观察时长(ms)、keydown 数、若干比例/计数、t0/t1、指针移动数、input 数 等。
// 我们只做两件事：把摘要写进日志，以及在“明明模拟了输入却一次交互都没记到”时失败即关闭
// ——零交互的会话会在数分钟后被异步风控撤销 access token。

// sentinelTErrRe 匹配 dx 程序在假环境里崩溃时上报的文本：`<已执行指令数>: TypeError: ...`。
var sentinelTErrRe = regexp.MustCompile(`^\d+: \w*(Error|Exception)`)

// checkSentinelT 校验令牌里的 t（turnstile.dx 的环境指纹输出）。
// 真实浏览器的 t 是 1.3~1.6KB 的 XOR 混淆 JSON；假环境缺 API 时 dx 程序会中途抛错，
// 上报的就只是一条几十字节的 "171: TypeError: ..."。这样的令牌同步校验能过，
// 但账号会在数分钟后被异步风控撤销 access token，所以宁可失败也不提交。
func checkSentinelT(token string) error {
	var obj struct {
		T string `json:"t"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(token)), &obj) != nil || strings.TrimSpace(obj.T) == "" {
		return nil
	}
	raw, err := decodeBase64Loose(obj.T)
	if err != nil {
		return fmt.Errorf("sentinel_t_undecodable")
	}
	if sentinelTErrRe.Match(raw) {
		msg := string(raw)
		if len(msg) > 90 {
			msg = msg[:90]
		}
		return fmt.Errorf("sentinel_t_error: %s", msg)
	}
	if len(raw) < 400 {
		return fmt.Errorf("sentinel_t_short(%d)", len(raw))
	}
	return nil
}

type soSnapshot struct {
	Fields []string
	Key    string
}

func xorCycle(b []byte, key string) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ key[i%len(key)]
	}
	return out
}

func decodeBase64Loose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}

// soPayload 取出 so 字段：SDK 返回的是 {"so":...,"c":...,"id":...,"flow":...} 这份 JSON
// （即 openai-sentinel-so-token 请求头的值），也兼容直接给裸 base64。
func soPayload(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") {
		var w struct {
			SO string `json:"so"`
		}
		if json.Unmarshal([]byte(s), &w) == nil && w.SO != "" {
			return w.SO
		}
	}
	return s
}

// decodeSessionObserver 尝试恢复 key 并按对象顺序返回各字段明文。
func decodeSessionObserver(so string) (*soSnapshot, bool) {
	raw, err := decodeBase64Loose(soPayload(so))
	if err != nil || len(raw) < 8 {
		return nil, false
	}
	// 明文必以 {" 开头、以 } 结尾，据此先做便宜的过滤。
	for a := 0; a < 100; a++ {
		for b := 0; b < 100; b++ {
			key := fmt.Sprintf("%d.%02d", a, b)
			if raw[0]^key[0] != '{' || raw[1]^key[1%len(key)] != '"' || raw[len(raw)-1]^key[(len(raw)-1)%len(key)] != '}' {
				continue
			}
			plain := xorCycle(raw, key)
			if !json.Valid(plain) {
				continue
			}
			fields, ok := orderedSOValues(plain, key)
			if ok {
				return &soSnapshot{Fields: fields, Key: key}, true
			}
		}
	}
	return nil, false
}

func orderedSOValues(plain []byte, key string) ([]string, bool) {
	dec := json.NewDecoder(bytes.NewReader(plain))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	var out []string
	for dec.More() {
		if _, err := dec.Token(); err != nil { // key
			return nil, false
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		b, err := decodeBase64Loose(v)
		if err != nil {
			return nil, false
		}
		out = append(out, string(xorCycle(b, key)))
	}
	return out, len(out) > 0
}

func (s *soSnapshot) num(i int) float64 {
	if s == nil || i < 0 || i >= len(s.Fields) {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(s.Fields[i]), 64)
	return f
}

// 字段位置来自对真实浏览器抓包的逐项对照（validate/create_account 两个 flow 一致）。
func (s *soSnapshot) DurationMs() float64 { return s.num(0) }
func (s *soSnapshot) KeyDowns() int       { return int(s.num(1)) }
func (s *soSnapshot) PointerMoves() int   { return int(s.num(15)) }

// Empty 表示这份快照里没有像样的真实交互：零按键，且指针移动不足 5 次。
// 被撤销的真实对照号（脚本填表、无真实输入事件）快照正是 keydown=0、pointermove=1。
func (s *soSnapshot) Empty() bool { return s != nil && s.KeyDowns() == 0 && s.PointerMoves() < 5 }

func (s *soSnapshot) String() string {
	if s == nil {
		return "so=unreadable"
	}
	return fmt.Sprintf("so_window=%.1fs keydown=%d pointermove=%d fields=%d", s.DurationMs()/1000, s.KeyDowns(), s.PointerMoves(), len(s.Fields))
}
