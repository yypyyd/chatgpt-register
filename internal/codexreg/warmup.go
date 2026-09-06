package codexreg

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
)

const (
	// promptSel ChatGPT 主界面输入框。前端隔几周就换一套 DOM：有过 textarea[name=prompt-textarea]、
	// contenteditable 的 #prompt-textarea、ProseMirror 编辑器、以及 form 里唯一的 contenteditable。
	// 这里按多个候选依次匹配，具体哪一个可见由 promptBoxVisibleJS 判断。
	promptSel = "#prompt-textarea, textarea[name='prompt-textarea'], div.ProseMirror[contenteditable='true'], form [contenteditable='true'], form textarea"

	warmupTimeout    = 2 * time.Minute
	warmupAnswerWait = 75 * time.Second
)

// warmupPrompts 注册后在注册浏览器里发一条最普通的问题。账号的第一次使用来自注册时的同一浏览器、
// 同一出口 IP、带完整的前端遥测，而不是几分钟后由另一台服务器直接调接口。
var warmupPrompts = []string{
	"Hi! Can you give me one quick tip for staying focused while studying?",
	"What's a good name for a golden retriever puppy?",
	"Suggest three easy dinner ideas for tonight.",
	"Explain in one sentence why the sky is blue.",
	"Can you recommend a good book for a long flight?",
	"How do I politely decline a meeting invite?",
	"What's the difference between a latte and a cappuccino?",
	"Give me a short motivational quote for Monday morning.",
	"How many minutes should I boil an egg for a runny yolk?",
	"What are some fun weekend activities for a rainy day?",
	"Help me write a two-line birthday message for a coworker.",
	"What's a simple stretching routine I can do at my desk?",
}

func randomPrompt() string { return warmupPrompts[ri(len(warmupPrompts))] }

// warmupChat 在主界面发送一条问题并等待回复。
func warmupChat(page *rod.Page, in Input) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("预热异常: %v", r)
		}
	}()
	pg := page.Timeout(warmupTimeout)
	// 真人进主界面会先看一眼。
	pause(1500*time.Millisecond, 3500*time.Millisecond)
	prompt := randomPrompt()

	// 新账号第一次发消息时，ChatGPT 会弹出 "You're all set / 準備が完了しました" 的条款确认页
	// （点「Continue」才算同意条款）。它可能在页面加载后立刻出现，也可能在按下回车那一刻才出现并
	// 吞掉这次发送，所以发送前后都要处理它，被吞掉就重发一次。
	for attempt := 0; attempt < 3; attempt++ {
		if err := dismissOnboarding(pg, in); err != nil {
			return err
		}
		box, err := waitPromptBox(pg, in, 20*time.Second)
		if err != nil {
			return fmt.Errorf("找不到输入框: %w", err)
		}
		if err := HumanType(pg, box, prompt, attempt > 0); err != nil {
			return fmt.Errorf("输入问题失败: %w", err)
		}
		// 确认文字真的进了输入框（落点被遮罩吃掉时会一个字都没有）。
		if v, err := box.Eval(`() => (this.value || this.innerText || '').trim()`); err == nil && len(v.Value.Str()) < len(prompt)/2 {
			digest, _ := pg.Timeout(3 * time.Second).Eval(pageDigestJS)
			return fmt.Errorf("输入框未接收到文字（当前内容 %q），页面: %s", v.Value.Str(), digest.Value.Str())
		}
		// 输入过程中可能已经弹出条款确认层（第一次敲键就触发），此时回车会敲到遮罩上。
		if st, ok := readChatState(pg); ok && stateHas(st, "gate") {
			in.logf("💬 输入时弹出条款确认页，先点掉再发送")
			_ = dismissOnboarding(pg, in)
			if !promptBoxVisible(pg) {
				continue
			}
			// 点掉后输入框里的内容有可能被清掉，重新聚焦并按需补齐。
			if el, err := pg.Timeout(3 * time.Second).ElementByJS(rod.Eval(promptBoxJS)); err == nil && el != nil {
				if v, err := el.Eval(`() => (this.value || this.innerText || '').trim()`); err == nil && len(v.Value.Str()) < len(prompt)/2 {
					if err := HumanType(pg, el, prompt, true); err != nil {
						return fmt.Errorf("重新输入问题失败: %w", err)
					}
				} else {
					_ = HumanClick(pg, el)
				}
			}
		}
		pause(400*time.Millisecond, 1200*time.Millisecond)
		if err := pg.Keyboard.Type(input.Enter); err != nil {
			return fmt.Errorf("发送失败: %w", err)
		}
		in.logf("💬 已发送预热问题，等待回复...")

		outcome, st := waitAnswerOrGate(pg, in)
		switch outcome {
		case "answered":
			return nil
		case "gate":
			in.logf("💬 发送后弹出条款确认页，点掉后检查消息是否已发出")
			_ = dismissOnboarding(pg, in)
			// 点完「继续」有的版本会把刚才那条消息自动发出去，等一小会看结果。
			if o2, _ := waitAnswerFor(pg, 25*time.Second); o2 == "answered" {
				return nil
			}
			if messageSent(pg) {
				o3, st3 := waitAnswerFor(pg, warmupAnswerWait)
				if o3 == "answered" {
					return nil
				}
				return fmt.Errorf("消息已发出但等待回复超时（状态 %s）", st3)
			}
			in.logf("💬 消息未发出，重新输入并发送")
			continue
		default:
			return fmt.Errorf("等待回复超时（最后状态 %s）", st)
		}
	}
	return fmt.Errorf("多次被条款确认页打断，放弃预热")
}

// chatStateJS 一次 Eval 读出对话页当前状态。选择器随 ChatGPT 前端改版会变，
// 因此同时按 data-* 属性、URL 变化（/c/<id>）和输入框可见性等多条线索判断。
const chatStateJS = `() => {` + chatHelpersJS + `
		const q = s => document.querySelector(s);
		const assistant = !!q("[data-message-author-role='assistant'], article[data-turn='assistant']");
		const stop = !!q("button[data-testid='stop-button'], button[aria-label*='Stop' i], button[aria-label*='停止']");
		const conv = /\/c\/[0-9a-f-]{8,}/i.test(location.pathname);
		const userTurn = !!q("[data-message-author-role='user'], article[data-turn='user']");
		const boxVisible = !!findPromptBox();
		const gate = !!gateButton();
		return JSON.stringify({assistant, stop, conv, userTurn, boxVisible, gate});
	}`

func readChatState(pg *rod.Page) (string, bool) {
	v, err := pg.Timeout(5 * time.Second).Eval(chatStateJS)
	if err != nil {
		return "", false
	}
	return v.Value.Str(), true
}

func stateHas(st, key string) bool { return strings.Contains(st, `"`+key+`":true`) }

// messageSent 消息是否已经发出（进入了会话 URL 或出现了用户消息气泡）。
func messageSent(pg *rod.Page) bool {
	st, ok := readChatState(pg)
	return ok && (stateHas(st, "conv") || stateHas(st, "userTurn"))
}

// waitAnswerOrGate 等回复；期间若弹出条款确认页则立即返回 "gate"。
func waitAnswerOrGate(pg *rod.Page, in Input) (string, string) {
	deadline := time.Now().Add(warmupAnswerWait)
	sentAt := time.Now()
	lastState := ""
	for time.Now().Before(deadline) {
		if st, ok := readChatState(pg); ok {
			lastState = st
			if stateHas(st, "assistant") && !stateHas(st, "stop") {
				pause(2500*time.Millisecond, 6000*time.Millisecond)
				return "answered", st
			}
			if stateHas(st, "gate") {
				return "gate", st
			}
			if (stateHas(st, "conv") || stateHas(st, "userTurn")) && !stateHas(st, "stop") && time.Since(sentAt) > 20*time.Second {
				in.logf("💬 回复节点未识别，但会话已建立且生成已结束，按完成处理（状态 %s）", st)
				pause(1500*time.Millisecond, 3000*time.Millisecond)
				return "answered", st
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return "timeout", lastState
}

// waitAnswerFor 在给定时长内等回复出现并生成完毕。
func waitAnswerFor(pg *rod.Page, d time.Duration) (string, string) {
	deadline := time.Now().Add(d)
	lastState := ""
	for time.Now().Before(deadline) {
		if st, ok := readChatState(pg); ok {
			lastState = st
			if stateHas(st, "assistant") && !stateHas(st, "stop") {
				pause(2500*time.Millisecond, 6000*time.Millisecond)
				return "answered", st
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return "timeout", lastState
}

// chatHelpersJS 页面侧公共函数，拼在各段 JS 前面：
//   - findPromptBox()：按候选选择器找"真正可见、可点到"的聊天输入框。主界面就绪时 DOM 里可能先挂一个
//     隐藏的 textarea，而上面还盖着整页的条款确认层，所以除了几何尺寸还要用 elementFromPoint 确认落点
//     确实是输入框本身（或同一 form 内），否则 Enter 会敲到遮罩上；
//   - gateButton()：条款确认 / 欢迎 / 引导层上的主按钮。文案随界面语言变化，不靠文案：优先对话框里的按钮，
//     否则取屏幕中央附近、顶层可点到、够大的按钮（排除顶部 Chat/Work 之类的分段切换）。
const chatHelpersJS = `
	const visible = el => { const r = el.getBoundingClientRect(); const s = getComputedStyle(el); return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none' && Number(s.opacity || 1) > 0; };
	const onTop = el => { const r = el.getBoundingClientRect(); const h = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2); return !!h && (h === el || el.contains(h) || (h.closest && el.closest('form') && h.closest('form') === el.closest('form'))); };
	const findPromptBox = () => {
		const cands = [...document.querySelectorAll("#prompt-textarea, textarea[name='prompt-textarea'], div.ProseMirror[contenteditable='true'], form [contenteditable='true'], form textarea")];
		return cands.find(b => { const r = b.getBoundingClientRect(); return r.width > 40 && r.height > 10 && visible(b) && onTop(b); }) || null;
	};
	const gateButton = () => {
		const dlg = document.querySelector("div[role='dialog'], [data-testid='modal']");
		if (dlg && visible(dlg)) {
			const bs = [...dlg.querySelectorAll('button')].filter(visible).filter(b => (b.innerText || '').trim().length > 0);
			if (bs.length) return bs[bs.length - 1];
		}
		if (findPromptBox()) return null;
		const cx = innerWidth / 2, cy = innerHeight / 2;
		const bs = [...document.querySelectorAll('button, a[role="button"]')].filter(visible).filter(onTop)
			.filter(b => { const r = b.getBoundingClientRect(); return r.width >= 120 && r.height >= 30 && (b.innerText || '').trim().length > 0
				&& Math.abs(r.x + r.width / 2 - cx) < innerWidth * 0.3 && r.y > innerHeight * 0.2 && r.y < innerHeight * 0.85; })
			.sort((a, b) => { const ra = a.getBoundingClientRect(), rb = b.getBoundingClientRect();
				return Math.hypot(ra.x + ra.width/2 - cx, ra.y + ra.height/2 - cy) - Math.hypot(rb.x + rb.width/2 - cx, rb.y + rb.height/2 - cy); });
		return bs.length ? bs[0] : null;
	};
`

const promptBoxJS = `() => {` + chatHelpersJS + ` return findPromptBox(); }`
const promptBoxVisibleJS = `() => {` + chatHelpersJS + ` return !!findPromptBox(); }`
const onboardingButtonJS = `() => {` + chatHelpersJS + ` return gateButton(); }`

// pageDigest 页面标题 / URL / 可见按钮摘要，预热失败时写日志用。
const pageDigestJS = `() => {
	const visible = el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
	const btns = [...document.querySelectorAll('button')].filter(visible).slice(0, 8)
		.map(b => ((b.innerText || b.getAttribute('aria-label') || '').replace(/\s+/g, ' ').trim().slice(0, 24) || '(无文案)'));
	const h = [...document.querySelectorAll('h1,h2')].filter(visible).map(x => (x.innerText || '').trim().slice(0, 40));
	return location.pathname + ' | h=' + JSON.stringify(h) + ' | buttons=' + JSON.stringify(btns);
}`

func promptBoxVisible(pg *rod.Page) bool {
	v, err := pg.Timeout(3 * time.Second).Eval(promptBoxVisibleJS)
	return err == nil && v.Value.Bool()
}

// dismissOnboarding 点掉欢迎页 / 条款确认页 / 引导弹窗，直到聊天输入框真正可见（最多几轮）。
func dismissOnboarding(pg *rod.Page, in Input) error {
	for round := 0; round < 5; round++ {
		if promptBoxVisible(pg) {
			return nil
		}
		el, err := pg.Timeout(4 * time.Second).ElementByJS(rod.Eval(onboardingButtonJS))
		if err != nil || el == nil {
			// 没有可点的按钮：可能只是主界面还在加载，按一次 Esc 关掉可能存在的浮层再等。
			_ = pg.Keyboard.Type(input.Escape)
			time.Sleep(1500 * time.Millisecond)
			continue
		}
		txt, _ := el.Text()
		in.logf("💬 点掉引导页按钮「%s」", strings.TrimSpace(txt))
		// 真人会先读一下条款说明。
		pause(1200*time.Millisecond, 2600*time.Millisecond)
		_ = HumanClick(pg, el)
		pause(1500*time.Millisecond, 3000*time.Millisecond)
	}
	return nil
}

// waitPromptBox 等聊天输入框真正可见（引导页点掉后主界面还要加载一会）。
func waitPromptBox(pg *rod.Page, in Input, timeout time.Duration) (*rod.Element, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if el, err := pg.Timeout(3 * time.Second).ElementByJS(rod.Eval(promptBoxJS)); err == nil && el != nil {
			return el, nil
		}
		time.Sleep(700 * time.Millisecond)
	}
	digest := ""
	if v, err := pg.Timeout(3 * time.Second).Eval(pageDigestJS); err == nil {
		digest = v.Value.Str()
	}
	return nil, fmt.Errorf("等待 %s 超时，页面: %s", timeout, digest)
}
