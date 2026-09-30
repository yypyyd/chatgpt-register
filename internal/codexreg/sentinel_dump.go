package codexreg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dumpSentinelExchange 仅在设置 SENTINEL_DEBUG_DUMP=<目录> 时把 sentinel/req 的原始往返
// （请求体 p + 响应体里的 dx/collector/snapshot 程序）追加到 <目录>/sentinel_req.jsonl，
// 供离线复现 dx 程序在假环境里的行为。默认关闭。
func dumpSentinelExchange(url, reqBody, respBody string) {
	dir := os.Getenv("SENTINEL_DEBUG_DUMP")
	if dir == "" || !strings.Contains(url, "sentinel/req") {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, "sentinel_req.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec, _ := json.Marshal(map[string]any{"ts": time.Now().Unix(), "url": url, "req": reqBody, "resp": respBody})
	_, _ = f.Write(append(rec, '\n'))
}

// dumpSentinelTokens 仅在设置 SENTINEL_DEBUG_DUMP=<目录> 时把签发的 token 追加到
// <目录>/sentinel_dump.jsonl，用于离线对比“真实浏览器 vs Node 假环境”的 p/t/so 字段。
// 默认关闭，生产不会写任何东西。
func dumpSentinelTokens(flow, token, so string) {
	dir := os.Getenv("SENTINEL_DEBUG_DUMP")
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	f, err := os.OpenFile(filepath.Join(dir, "sentinel_dump.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec, _ := json.Marshal(map[string]any{"ts": time.Now().Unix(), "flow": flow, "token": token, "so": so})
	_, _ = f.Write(append(rec, '\n'))
}
