package codexreg

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// pause 在 [min,max] 内随机停顿。真人在两步操作之间总有反应时间，
// "零延迟"连续操作是前端行为风控最容易识别的特征。
func pause(min, max time.Duration) {
	if max <= min {
		time.Sleep(min)
		return
	}
	time.Sleep(min + time.Duration(ri(int(max-min))))
}

// rf 返回 [0,1) 的随机浮点。
func rf() float64 { return float64(ri(1<<20)) / float64(1<<20) }

// HumanClick 用真实鼠标事件点击：光标先沿带缓动的轨迹分段移到元素内一个随机点，再按下/抬起。
// 这样产生的事件 isTrusted=true 且带完整的 mousemove 序列；JS 的 element.click() 生成的事件
// isTrusted=false，只在拿不到元素几何信息（被遮挡/不可见）时才兜底使用。
func HumanClick(page *rod.Page, el *rod.Element) error {
	if el == nil {
		return errors.New("nil element")
	}
	scrollIntoView(el)
	pause(150*time.Millisecond, 400*time.Millisecond)
	if pt, ok := randomPointIn(el); ok {
		if err := moveMouseHuman(page, pt); err == nil {
			pause(60*time.Millisecond, 160*time.Millisecond)
			if err := page.Mouse.Down(proto.InputMouseButtonLeft, 1); err == nil {
				pause(50*time.Millisecond, 130*time.Millisecond)
				if err := page.Mouse.Up(proto.InputMouseButtonLeft, 1); err == nil {
					return nil
				}
			}
		}
	}
	_, err := el.Eval(`() => this.click()`)
	return err
}

// scrollIntoView 直接用 DOM 调用滚到元素，不经 rod 的 ScrollIntoView：后者内部先
// WaitStableRAF，遇到带持续动画（转圈图标、渐入）的页面会一直等到 ctx 超时。
func scrollIntoView(el *rod.Element) {
	_, _ = el.Eval(`() => this.scrollIntoView({block: 'center', inline: 'nearest'})`)
}

// focusElement 直接聚焦，同样绕开 rod Focus 里的 ScrollIntoView/WaitStableRAF。
func focusElement(el *rod.Element) error {
	_, err := el.Eval(`() => this.focus()`)
	return err
}

// randomPointIn 取元素可见区域内偏离中心的一个随机点（避开边缘 20%）。
func randomPointIn(el *rod.Element) (proto.Point, bool) {
	shape, err := el.Shape()
	if err != nil || shape == nil {
		return proto.Point{}, false
	}
	box := shape.Box()
	if box == nil || box.Width < 2 || box.Height < 2 {
		return proto.Point{}, false
	}
	return proto.Point{
		X: box.X + box.Width*(0.2+0.6*rf()),
		Y: box.Y + box.Height*(0.2+0.6*rf()),
	}, true
}

// moveMouseHuman 把光标从当前位置移到目标：smoothstep 缓动 + 少量抖动 + 每步随机间隔。
func moveMouseHuman(page *rod.Page, to proto.Point) error {
	from := page.Mouse.Position()
	if from.X == 0 && from.Y == 0 {
		// 新页面光标还在原点；先落到视口里某处，避免每次都从 (0,0) 直线出发。
		from = proto.Point{X: 200 + 600*rf(), Y: 150 + 300*rf()}
		if err := page.Mouse.MoveTo(from); err != nil {
			return err
		}
		pause(40*time.Millisecond, 120*time.Millisecond)
	}
	dist := math.Hypot(to.X-from.X, to.Y-from.Y)
	steps := 10 + int(dist/40) + ri(6)
	if steps > 40 {
		steps = 40
	}
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		e := t * t * (3 - 2*t)
		pt := to
		if i < steps {
			pt = proto.Point{
				X: from.X + (to.X-from.X)*e + (rf()-0.5)*2,
				Y: from.Y + (to.Y-from.Y)*e + (rf()-0.5)*2,
			}
		}
		if err := page.Mouse.MoveTo(pt); err != nil {
			return err
		}
		time.Sleep(time.Duration(6+ri(12)) * time.Millisecond)
	}
	return nil
}

// HumanType 先点击聚焦，再逐字符发送 keydown/keyup（需要 Shift 的字符带 Shift 修饰），
// 字符间隔随机、偶尔停顿更久。rod 默认的 Input 是一次性 insertText，没有任何按键事件，
// 与真人打字完全不同。clear=true 时先清空已有内容。
func HumanType(page *rod.Page, el *rod.Element, text string, clear bool) error {
	if el == nil {
		return errors.New("nil element")
	}
	if err := page.GetContext().Err(); err != nil {
		return err
	}
	if err := HumanClick(page, el); err != nil {
		if ferr := focusElement(el); ferr != nil {
			return ferr
		}
	}
	// 点击落点被浮层 / label 吃掉时输入框不会获得焦点，补一次 focus 再打字。
	if focused, ferr := el.Eval(`() => document.activeElement === this`); ferr != nil || !focused.Value.Bool() {
		_ = focusElement(el)
	}
	pause(200*time.Millisecond, 500*time.Millisecond)
	if clear {
		// 全选 + 退格，与真人清空输入框一致（不用 rod SelectAllText，它内部会再走一遍 Focus/ScrollIntoView）。
		_, _ = el.Eval(`() => { if (typeof this.select === 'function') this.select(); else { const r = document.createRange(); r.selectNodeContents(this); const s = getSelection(); s.removeAllRanges(); s.addRange(r); } }`)
		_ = page.Keyboard.Type(input.Backspace)
		pause(80*time.Millisecond, 200*time.Millisecond)
	}
	for _, r := range text {
		if err := typeRune(page, r); err != nil {
			return err
		}
		if ri(14) == 0 {
			pause(220*time.Millisecond, 600*time.Millisecond)
		} else {
			pause(55*time.Millisecond, 150*time.Millisecond)
		}
	}
	return nil
}

const shiftSymbols = "~!@#$%^&*()_+{}|:\"<>?"

// typeRune 按一个键。rod 的键位表只覆盖 US 键盘可打出的字符，其它字符退回 insertText。
func typeRune(page *rod.Page, r rune) error {
	key := input.Key(r)
	var info input.KeyInfo
	if rod.Try(func() { info = key.Info() }) != nil {
		return page.InsertText(string(r))
	}
	// 主键盘区的大写字母 / 上排符号需要按住 Shift；小键盘键（如 NumpadAdd）不用。
	shifted := info.Location != 3 && (unicode.IsUpper(r) || strings.ContainsRune(shiftSymbols, r))
	if shifted {
		if err := page.Keyboard.Press(input.ShiftLeft); err != nil {
			return err
		}
		pause(20*time.Millisecond, 60*time.Millisecond)
	}
	err := page.Keyboard.Type(key)
	if shifted {
		pause(15*time.Millisecond, 50*time.Millisecond)
		_ = page.Keyboard.Release(input.ShiftLeft)
	}
	return err
}
