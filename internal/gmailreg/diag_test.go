package gmailreg

import (
	"strings"
	"testing"
	"time"
)

// TestDiagnoseBotDetection 用当前的 stealth 浏览器访问机器人检测站,
// 打印检测结果,定位哪些自动化信号仍在泄露。
// 运行: go test ./internal/gmailreg/ -run TestDiagnoseBotDetection -v -timeout 120s
func TestDiagnoseBotDetection(t *testing.T) {
	in := Input{
		Headless: false,
		Log:      func(f string, a ...any) { t.Logf(f, a...) },
	}
	browser, bridge, cleanup, err := launchStealthBrowser(in)
	if err != nil {
		t.Fatalf("启动浏览器失败: %v", err)
	}
	defer browser.Close()
	if bridge != nil {
		defer bridge.Close()
	}
	if cleanup != nil {
		defer cleanup()
	}

	fp := randomFingerprint()
	page := browser.MustPage("")
	applyStealth(page, browser, fp, "us")

	// rebrowser 的检测页,专测 Runtime.enable / webdriver / CDP 等
	if err := page.Navigate("https://bot-detector.rebrowser.net/"); err != nil {
		t.Fatalf("导航失败: %v", err)
	}
	page.MustWaitLoad()
	time.Sleep(8 * time.Second) // 等检测跑完

	// 抓页面上所有检测项的文本
	res, err := page.Eval(`() => {
		return document.body ? document.body.innerText : '(no body)';
	}`)
	if err != nil {
		t.Fatalf("读取结果失败: %v", err)
	}
	text := res.Value.Str()
	t.Logf("====== bot-detector.rebrowser.net 结果 ======\n%s", text)

	// 额外:直接检查几个关键信号
	checks, _ := page.Eval(`() => {
		const out = {};
		out.webdriver = navigator.webdriver;
		out.webgl_vendor = (() => { try { const c = document.createElement('canvas').getContext('webgl'); const d = c.getExtension('WEBGL_debug_renderer_info'); return c.getParameter(d.UNMASKED_VENDOR_WEBGL); } catch(e){ return 'err:'+e.message; } })();
		out.plugins = navigator.plugins.length;
		out.languages = JSON.stringify(navigator.languages);
		out.hardwareConcurrency = navigator.hardwareConcurrency;
		out.hasChrome = !!window.chrome;
		out.hasRTC = typeof RTCPeerConnection;
		return JSON.stringify(out, null, 2);
	}`)
	if checks != nil {
		t.Logf("====== 关键信号 ======\n%s", checks.Value.Str())
	}

	if strings.Contains(text, "FAIL") || strings.Contains(text, "detected") {
		t.Logf("⚠️ 检测到自动化泄露")
	}
}
