// Package turnstilepatch 内置一个极小的 Chrome 扩展：在每个 frame 的 document_start
// 改写 MouseEvent.screenX/screenY，让 Cloudflare managed Turnstile 认为复选框是真人
// 点的从而签发 token，不依赖第三方打码。grokreg / leonardoreg 共用同一份扩展。
package turnstilepatch

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed assets/manifest.json assets/script.js
var assets embed.FS

// Extract 把扩展释放到一个新建的临时目录并返回路径，调用方负责 os.RemoveAll。
// pattern 是 os.MkdirTemp 的目录名模式，用于区分不同平台的临时目录。
func Extract(pattern string) (string, error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"manifest.json", "script.js"} {
		data, rerr := assets.ReadFile("assets/" + name)
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
