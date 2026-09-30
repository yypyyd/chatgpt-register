package codexreg

import (
	"fmt"
)

// ScreenProfile 一套桌面屏幕规格，写入 auth_data 供下游沿用同一指纹。
type ScreenProfile struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	// Taskbar 系统任务栏高度；Toolbar 浏览器标签栏 + 地址栏（+ 书签栏）高度。
	Taskbar int `json:"taskbar"`
	Toolbar int `json:"toolbar"`
	// DPR 设备像素比：Windows 显示缩放 100%/125%/150% 对应 1/1.25/1.5，与 CSS 像素屏幕尺寸配套
	// （例如 1920x1080@125% 的 CSS 尺寸是 1536x864）。文本测量指纹按 DPR 取整，必须与屏幕自洽。
	DPR float64 `json:"dpr,omitempty"`
	// 硬件档位：navigator.hardwareConcurrency / deviceMemory / maxTouchPoints。
	HWConcurrency int `json:"hwc,omitempty"`
	DeviceMemory  int `json:"mem,omitempty"`
	MaxTouch      int `json:"touch,omitempty"`
}

func (p ScreenProfile) valid() bool {
	return p.Width >= 1024 && p.Height >= 600 && p.Taskbar >= 0 && p.Toolbar > 0 && p.ViewportHeight() > 400
}

func (p ScreenProfile) WindowHeight() int { return p.Height - p.Taskbar }

func (p ScreenProfile) ViewportHeight() int { return p.WindowHeight() - p.Toolbar }

func (p ScreenProfile) String() string { return fmt.Sprintf("%dx%d", p.Width, p.Height) }

var screenPool = []struct{ w, h, weight int }{
	{1920, 1080, 34},
	{1366, 768, 14},
	{1536, 864, 12},
	{2560, 1440, 9},
	{1440, 900, 8},
	{1600, 900, 6},
	{1280, 720, 5},
	{1280, 800, 4},
	{1680, 1050, 3},
	{1920, 1200, 3},
	{1600, 1200, 2},
}

func pickScreenProfile() ScreenProfile {
	total := 0
	for _, s := range screenPool {
		total += s.weight
	}
	pick := ri(total)
	p := ScreenProfile{Width: screenPool[0].w, Height: screenPool[0].h}
	for _, s := range screenPool {
		if pick < s.weight {
			p.Width, p.Height = s.w, s.h
			break
		}
		pick -= s.weight
	}
	// UA / Sentinel 窗口都按 Windows Chrome 报。注册跑在 Linux 上也不能用 Linux 任务栏高度。
	p.Taskbar = []int{40, 48}[ri(2)]
	p.Toolbar = []int{85, 120}[ri(2)] + ri(6)
	p.DPR = dprForScreen(p.Width, p.Height)
	p.HWConcurrency, p.DeviceMemory, p.MaxTouch = pickHardware()
	return p
}

// dprForScreen 给出与 CSS 像素屏幕尺寸自洽的 Windows 显示缩放。
func dprForScreen(w, h int) float64 {
	switch {
	case w == 1536 && h == 864: // 1920x1080 @125%
		return 1.25
	case w == 1280 && h == 720: // 1920x1080 @150%
		return 1.5
	case w == 1280 && h == 800: // 1920x1200 @150%
		return 1.5
	case w == 1600 && h == 900 && ri(100) < 30: // 少数 1600x900 是 2000x1125@125% 之类的高分屏，多数是原生 1600x900
		return 1.25
	default:
		return 1
	}
}

// pickHardware 按常见 Windows 桌面/笔记本的分布挑一组自洽的核数/内存/触点。
// Chrome 新版本的 deviceMemory 不再封顶 8，会给出 16/32。
func pickHardware() (hwc, mem, touch int) {
	r := ri(100)
	switch {
	case r < 40:
		mem = 8
		hwc = []int{4, 6, 8}[ri(3)]
	case r < 75:
		mem = 16
		hwc = []int{8, 12, 16}[ri(3)]
	default:
		mem = 32
		hwc = []int{12, 16, 20, 24}[ri(4)]
	}
	if ri(100) < 8 { // 触屏笔记本
		touch = 10
	}
	return hwc, mem, touch
}
