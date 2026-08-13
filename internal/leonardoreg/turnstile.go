package leonardoreg

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	// turnstileRoundWait 点完一次等挑战出 token 的时间，超时就 reset 重来。
	turnstileRoundWait = 12 * time.Second
	// turnstileMaxRounds 最多点几轮：两轮不出 token 基本就是出口被判定了。
	turnstileMaxRounds = 2
	// turnstileClickWait 一直定位不到复选框（无感模式也没自动出 token）的等待上限。
	turnstileClickWait = 30 * time.Second
)

// waitTurnstile 等 Leonardo 登录页的 Cloudflare Turnstile 签发 token。
// 组件多数情况下会自己过（无感），需要点选时按 grokreg 的做法穿透 shadow DOM 点真实
// 复选框；不做任何绕过，拿不到 token 就按失败返回，由上层重试/换出口。
func waitTurnstile(ctx context.Context, page *rod.Page, in Input, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	start := time.Now()
	logged, clicked := false, false
	rounds := 0
	var lastClick time.Time
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if turnstileTokenLen(page.Timeout(5*time.Second)) >= 20 {
			in.logf("Turnstile 人机校验已通过")
			return nil
		}
		if !logged {
			in.logf("等待 Turnstile 人机校验")
			logged = true
		}
		// 点一次后等它跑挑战（中途重复点会打断），到点还没 token 就 reset 重新发起。
		// 出口被 Cloudflare 判定时再点也不会出 token，点满 turnstileMaxRounds 轮就
		// 早失败，让上层换出口重来，别把时间耗在这里。
		if clicked && time.Since(lastClick) > turnstileRoundWait {
			if rounds >= turnstileMaxRounds {
				break
			}
			resetTurnstile(page.Timeout(5 * time.Second))
			clicked = false
			time.Sleep(2 * time.Second)
			continue
		}
		if !clicked {
			// 既没自动出 token、也点不到复选框，多等无益，早失败让上层换出口。
			if rounds == 0 && time.Since(start) > turnstileClickWait {
				break
			}
			if how := clickTurnstileCheckbox(page.Timeout(15 * time.Second)); how != "" {
				in.logf("已点选 Turnstile 复选框(%s)，等待签发 token", how)
				clicked = true
				rounds++
				lastClick = time.Now()
			}
		}
		time.Sleep(time.Second)
	}
	if clicked {
		return fmt.Errorf("Turnstile 已点选但未签发 token，可能需要更干净的代理出口")
	}
	return fmt.Errorf("Turnstile 人机校验超时未通过")
}

// turnstileTokenLen 读取隐藏域 / turnstile.getResponse() 里的 token，并回写到
// cf-turnstile-response（同 grokreg），让表单能读到；返回 token 长度。
func turnstileTokenLen(page *rod.Page) int {
	v, err := page.Eval(`() => {
		const input = document.querySelector('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]');
		let token = String((input && input.value) || '').trim();
		if (token.length < 20) {
			try {
				if (window.turnstile && typeof window.turnstile.getResponse === 'function') {
					token = String(window.turnstile.getResponse() || '').trim();
				}
			} catch (e) {}
		}
		if (token.length >= 20 && input) {
			const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set
				|| Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')?.set;
			if (setter) setter.call(input, token); else input.value = token;
			input.dispatchEvent(new Event('input', { bubbles: true }));
			input.dispatchEvent(new Event('change', { bubbles: true }));
			try { if (typeof window.__cfSolve === 'function') window.__cfSolve(token); } catch (e) {}
		}
		return token.length;
	}`)
	if err != nil {
		return 0
	}
	return int(v.Value.Num())
}

// resetTurnstile 重置组件，让它重新发起挑战（组件超时后必须先 reset 才可能签发 token）。
func resetTurnstile(page *rod.Page) {
	_, _ = page.Eval(`() => {
		try { if (window.turnstile && typeof window.turnstile.reset === 'function') window.turnstile.reset(); } catch (e) {}
	}`)
}

// clickTurnstileCheckbox 点 Turnstile 的复选框。组件的挑战 iframe 挂在 closed shadow
// root 里，页面脚本查不到，只能用 CDP 穿透拿它的位置再按坐标点。
func clickTurnstileCheckbox(page *rod.Page) string {
	box, ok := turnstileIframeBox(page)
	if !ok || box.W < 60 || box.H < 40 {
		return tryTurnstileClick(page)
	}
	// CDP 合成点击的 screenX/screenY 与真实光标不一致，交互式挑战会直接报 600010，
	// 有真实窗口时先用 X11 光标点。
	if how := x11ClickTurnstile(page, box); how != "" {
		return how
	}
	if mouseClickAt(page, box.X+21, box.Y+math.Min(28, box.H/2)) {
		return "iframe"
	}
	return tryTurnstileClick(page)
}

// hasTurnstileWidget 判断页面上是否还挂着 Turnstile 组件。
func hasTurnstileWidget(page *rod.Page) bool {
	return hasSel(page, `[name="cf-turnstile-response"]`)
}

// x11ClickTurnstile 用 xdotool 移动真实光标去点复选框：事件由 X 进入浏览器，
// screenX/screenY、trusted 都和真人一致。无显示环境或没装 xdotool 时返回空串。
func x11ClickTurnstile(page *rod.Page, box turnstileBox) string {
	if os.Getenv("DISPLAY") == "" {
		return ""
	}
	tool, err := exec.LookPath("xdotool")
	if err != nil {
		return ""
	}
	// window.screenX/screenY 是视口左上角在屏幕上的位置，加上标题栏/工具栏高度换算。
	v, err := page.Eval(`() => [window.screenX, window.screenY, window.outerHeight - window.innerHeight]`)
	if err != nil {
		return ""
	}
	arr := v.Value.Arr()
	if len(arr) < 3 || arr[2].Num() < 0 {
		return ""
	}
	x := arr[0].Num() + box.X + 21
	y := arr[1].Num() + arr[2].Num() + box.Y + math.Min(28, box.H/2)
	// 一个 X 显示上只有一个真光标，多个任务必须排队，点前还要把自己的窗口抬到最前。
	x11ClickMu.Lock()
	defer x11ClickMu.Unlock()
	if !x11ActivateWindowAt(tool, int(arr[0].Num()), int(arr[1].Num())) {
		return ""
	}
	// 先落到复选框附近再移进去点，轨迹更像真人。
	if err := exec.Command(tool, "mousemove",
		strconv.Itoa(int(x-40)), strconv.Itoa(int(y-30))).Run(); err != nil {
		return ""
	}
	time.Sleep(300 * time.Millisecond)
	if err := exec.Command(tool, "mousemove", strconv.Itoa(int(x)), strconv.Itoa(int(y)),
		"sleep", "0.2", "click", "1").Run(); err != nil {
		return ""
	}
	return "x11"
}

type turnstileBox struct {
	X, Y, W, H float64
}

// turnstileIframeBox 用 DOM.getDocument(pierce) 穿透 shadow DOM 找挑战 iframe 的位置。
func turnstileIframeBox(page *rod.Page) (turnstileBox, bool) {
	depth := -1
	doc, err := proto.DOMGetDocument{Depth: &depth, Pierce: true}.Call(page)
	if err != nil || doc == nil || doc.Root == nil {
		return turnstileBox{}, false
	}
	// 页面上同时挂着 Cloudflare 的 1x1 隐藏帧和真正的挑战组件，取面积最大的那个。
	var best turnstileBox
	found := false
	for _, node := range findTurnstileIframes(doc.Root, nil) {
		model, merr := proto.DOMGetBoxModel{NodeID: node.NodeID}.Call(page)
		if merr != nil || model == nil || model.Model == nil || len(model.Model.Content) < 4 {
			continue
		}
		q := model.Model.Content
		box := turnstileBox{X: q[0], Y: q[1], W: float64(model.Model.Width), H: float64(model.Model.Height)}
		if !found || box.W*box.H > best.W*best.H {
			best, found = box, true
		}
	}
	return best, found
}

// findTurnstileIframes 深度遍历（含 shadow root 与子文档）收集 challenges.cloudflare.com 的 iframe。
func findTurnstileIframes(node *proto.DOMNode, out []*proto.DOMNode) []*proto.DOMNode {
	if node == nil {
		return out
	}
	if strings.EqualFold(node.NodeName, "IFRAME") {
		for i := 0; i+1 < len(node.Attributes); i += 2 {
			if node.Attributes[i] == "src" && strings.Contains(node.Attributes[i+1], "challenges.cloudflare.com") {
				out = append(out, node)
			}
		}
	}
	for _, child := range node.Children {
		out = findTurnstileIframes(child, out)
	}
	for _, root := range node.ShadowRoots {
		out = findTurnstileIframes(root, out)
	}
	return findTurnstileIframes(node.ContentDocument, out)
}

// tryTurnstileClick 点击 Turnstile 的真实复选框：组件挂在 wrapper 的 shadow root 里，
// 复选框又在跨域 iframe body 自己的 shadow root 中，逐层穿透后直接点该元素；
// 穿透不到时才退回按坐标点 host 热区。返回非空字符串表示已点击（内容为路径）。
func tryTurnstileClick(page *rod.Page) (clicked string) {
	defer func() {
		if recover() != nil {
			clicked = ""
		}
	}()
	response, err := page.Element(`[name="cf-turnstile-response"]`)
	if err != nil || response == nil {
		return turnstileFallbackClick(page)
	}
	wrapper, err := response.Parent()
	if err != nil || wrapper == nil {
		return turnstileFallbackClick(page)
	}
	shadow, err := wrapper.ShadowRoot()
	if err != nil || shadow == nil || shadow.Page() == nil {
		return turnstileFallbackClick(page)
	}
	iframe, err := shadow.Element("iframe")
	if err != nil || iframe == nil || iframe.Page() == nil {
		return turnstileFallbackClick(page)
	}
	frame, err := iframe.Frame()
	if err != nil || frame == nil || frame.FrameID == "" {
		return turnstileFallbackClick(page)
	}
	// 跳域 iframe 里也伪造 screenX/screenY，让合成点击看起来像真光标（同 grokreg）。
	_, _ = frame.Eval(`() => {
		try {
			const sx = 800 + Math.floor(Math.random() * 400);
			const sy = 400 + Math.floor(Math.random() * 300);
			Object.defineProperty(MouseEvent.prototype, 'screenX', { configurable: true, get: () => sx });
			Object.defineProperty(MouseEvent.prototype, 'screenY', { configurable: true, get: () => sy });
		} catch (e) {}
	}`)
	body, err := frame.Element("body")
	if err != nil || body == nil || body.Page() == nil {
		return turnstileFallbackClick(page)
	}
	root := body
	if inner, innerErr := body.ShadowRoot(); innerErr == nil && inner != nil && inner.Page() != nil {
		root = inner
	}
	button, err := root.Element(`input, [role="checkbox"], label, button`)
	if err != nil || button == nil || button.Page() == nil {
		return turnstileFallbackClick(page)
	}
	if err := button.Click(proto.InputMouseButtonLeft, 1); err == nil {
		return "shadow"
	}
	if mouseClickElement(button) {
		return "shadow-mouse"
	}
	return turnstileFallbackClick(page)
}

// turnstileFallbackClick 按页面坐标点组件可见的 host 方框（穿透不到 shadow root 时兜底）。
func turnstileFallbackClick(page *rod.Page) string {
	point, err := page.Eval(`() => {
		const response = document.querySelector('[name="cf-turnstile-response"]');
		const host = response && (response.parentElement || {}).parentElement;
		if (!host) return null;
		const style = getComputedStyle(host);
		const r = host.getBoundingClientRect();
		if (style.display === 'none' || style.visibility === 'hidden' || Number(style.opacity || 1) === 0) return null;
		if (r.width < 100 || r.height < 40) return null;
		return { x: r.left + 21, y: r.top + Math.min(35, r.height / 2), w: r.width, h: r.height };
	}`)
	if err != nil || point == nil {
		return ""
	}
	raw, _ := json.Marshal(point.Value)
	var p struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	if json.Unmarshal(raw, &p) != nil || p.W < 100 || p.H < 40 {
		return ""
	}
	if mouseClickAt(page, p.X, p.Y) {
		return "coord"
	}
	return ""
}

// x11ClickMu 串行化真光标操作：显示器上只有一个光标，并发点击会互相串台。
var x11ClickMu sync.Mutex

// x11ActivateWindowAt 按窗口左上角坐标找到本任务的浏览器窗口并激活到最前；
// 找不到唯一匹配就返回 false，让调用方退回合成点击而不是点错窗口。
func x11ActivateWindowAt(tool string, left, top int) bool {
	out, err := exec.Command(tool, "search", "--onlyvisible", "--name", ".").Output()
	if err != nil {
		return false
	}
	match := ""
	for _, id := range strings.Fields(string(out)) {
		geom, gerr := exec.Command(tool, "getwindowgeometry", "--shell", id).Output()
		if gerr != nil {
			continue
		}
		wx, wy, ok := parseWindowOrigin(string(geom))
		if !ok || abs(wx-left) > 6 || abs(wy-top) > 6 {
			continue
		}
		if match != "" {
			// 两个窗口摆在同一处，点下去无法保证是自己的窗口。
			return false
		}
		match = id
	}
	if match == "" {
		return false
	}
	if err = exec.Command(tool, "windowactivate", "--sync", match).Run(); err != nil {
		if err = exec.Command(tool, "windowraise", match).Run(); err != nil {
			return false
		}
	}
	time.Sleep(300 * time.Millisecond)
	return true
}

// parseWindowOrigin 解析 xdotool getwindowgeometry --shell 的 X=/Y= 输出。
func parseWindowOrigin(shell string) (int, int, bool) {
	x, y := 0, 0
	gotX, gotY := false, false
	for _, line := range strings.Split(shell, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "X="):
			if v, err := strconv.Atoi(line[2:]); err == nil {
				x, gotX = v, true
			}
		case strings.HasPrefix(line, "Y="):
			if v, err := strconv.Atoi(line[2:]); err == nil {
				y, gotY = v, true
			}
		}
	}
	return x, y, gotX && gotY
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
