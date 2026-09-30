package codexreg

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// buildSO 按官方编码方式构造一份 so：base64(XOR(JSON{id: base64(XOR(value,K))}, K))。
func buildSO(t *testing.T, key string, values []string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteByte('{')
	for i, v := range values {
		if i > 0 {
			sb.WriteByte(',')
		}
		inner := base64.StdEncoding.EncodeToString(xorCycle([]byte(v), key))
		id, _ := json.Marshal(strings.Repeat("9", 1) + string(rune('0'+i%10)) + "." + string(rune('0'+(i/10)%10)) + "5")
		val, _ := json.Marshal(inner)
		sb.Write(id)
		sb.WriteByte(':')
		sb.Write(val)
	}
	sb.WriteByte('}')
	return base64.StdEncoding.EncodeToString(xorCycle([]byte(sb.String()), key))
}

func TestDecodeSessionObserverHuman(t *testing.T) {
	// 与真实浏览器 validate 快照同形：[0]时长 [1]keydown … [15]pointermove
	vals := make([]string, 29)
	for i := range vals {
		vals[i] = "0"
	}
	vals[0], vals[1], vals[15], vals[27] = "50002.59999999", "6", "121", "6"
	for _, key := range []string{"22.97", "85.87", "7.05"} {
		so := buildSO(t, key, vals)
		snap, ok := decodeSessionObserver(so)
		if !ok {
			t.Fatalf("key=%s decode failed", key)
		}
		if snap.Key != key {
			t.Fatalf("recovered key %q want %q", snap.Key, key)
		}
		if snap.KeyDowns() != 6 || snap.PointerMoves() != 121 || snap.Empty() {
			t.Fatalf("unexpected snapshot: %s", snap)
		}
		if got := snap.DurationMs(); got < 50000 || got > 50003 {
			t.Fatalf("duration=%v", got)
		}
	}
}

func TestDecodeSessionObserverJSONWrapper(t *testing.T) {
	vals := make([]string, 29)
	for i := range vals {
		vals[i] = "0"
	}
	vals[0], vals[1], vals[15] = "21244.6", "6", "172"
	so := buildSO(t, "85.87", vals)
	wrapped := `{"so":"` + so + `","c":"gAAAAAB...","id":"720250ac","flow":"email_otp_validate"}`
	snap, ok := decodeSessionObserver(wrapped)
	if !ok || snap.KeyDowns() != 6 || snap.PointerMoves() != 172 {
		t.Fatalf("wrapped so must decode, ok=%v snap=%v", ok, snap)
	}
}

func TestDecodeSessionObserverEmpty(t *testing.T) {
	vals := make([]string, 29)
	for i := range vals {
		vals[i] = "0"
	}
	vals[0] = "16043.70000001" // 真实 X6（零输入事件，随后被撤销）快照：只有时长与 1 次指针事件
	vals[15] = "1"
	snap, ok := decodeSessionObserver(buildSO(t, "79.14", vals))
	if !ok || !snap.Empty() {
		t.Fatalf("empty snapshot must be recognised, ok=%v snap=%v", ok, snap)
	}
}

func TestCheckSentinelT(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tok := func(tv string) string {
		b, _ := json.Marshal(map[string]string{"p": "gAAAAAB...", "t": tv, "c": "x", "id": "i", "flow": "f"})
		return string(b)
	}
	// 假环境崩溃时上报的文本（线上真实出现过）
	crash := tok(enc("171: TypeError: Cannot read properties of undefined (reading 'bind')"))
	if err := checkSentinelT(crash); err == nil || !strings.Contains(err.Error(), "sentinel_t_error") {
		t.Fatalf("crash text must be rejected, err=%v", err)
	}
	// 太短
	if err := checkSentinelT(tok(enc("short"))); err == nil {
		t.Fatal("short t must be rejected")
	}
	// 真实形态：约 1.1KB 的混淆字节
	long := make([]byte, 1100)
	for i := range long {
		long[i] = byte(i*7 + 3)
	}
	if err := checkSentinelT(tok(base64.StdEncoding.EncodeToString(long))); err != nil {
		t.Fatalf("real-shaped t must pass, err=%v", err)
	}
	// 没有 t 字段（turnstile 不要求）时不做校验
	if err := checkSentinelT(`{"p":"x","c":"y"}`); err != nil {
		t.Fatalf("missing t must pass, err=%v", err)
	}
}

func TestDecodeSessionObserverGarbage(t *testing.T) {
	if _, ok := decodeSessionObserver("not-base64!!"); ok {
		t.Fatal("garbage must not decode")
	}
	if _, ok := decodeSessionObserver(base64.StdEncoding.EncodeToString([]byte("plain text without json shape"))); ok {
		t.Fatal("non-json payload must not decode")
	}
}
