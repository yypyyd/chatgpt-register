package leonardoreg

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed turnstilepatch/manifest.json turnstilepatch/script.js
var turnstilePatchFS embed.FS

// extractTurnstilePatch 把内置的 Turnstile 补丁扩展释放到临时目录并返回路径。
// 扩展在每个 frame 的 document_start 改写 MouseEvent.screenX/screenY，让
// Cloudflare 的 managed Turnstile 认为复选框是真人点的从而签发 token，不依赖
// 第三方打码；与 grokreg 用的是同一份扩展。
func extractTurnstilePatch() (string, error) {
	dir, err := os.MkdirTemp("", "leonardo-turnstilepatch-*")
	if err != nil {
		return "", err
	}
	for _, name := range []string{"manifest.json", "script.js"} {
		data, rerr := turnstilePatchFS.ReadFile("turnstilepatch/" + name)
		if rerr != nil {
			os.RemoveAll(dir)
			return "", rerr
		}
		if werr := os.WriteFile(filepath.Join(dir, name), data, 0o644); werr != nil {
			os.RemoveAll(dir)
			return "", werr
		}
	}
	return dir, nil
}
