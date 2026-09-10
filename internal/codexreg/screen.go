package codexreg

import (
	"fmt"
	"runtime"
)

// ScreenProfile 一套桌面屏幕规格，写入 auth_data 供下游沿用同一指纹。
type ScreenProfile struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	// Taskbar 系统任务栏高度；Toolbar 浏览器标签栏 + 地址栏（+ 书签栏）高度。
	Taskbar int `json:"taskbar"`
	Toolbar int `json:"toolbar"`
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
	switch runtime.GOOS {
	case "windows":
		p.Taskbar = []int{40, 48}[ri(2)]
	case "darwin":
		p.Taskbar = 25 + []int{0, 70}[ri(2)]
	default:
		p.Taskbar = []int{0, 27, 32}[ri(3)]
	}
	p.Toolbar = []int{85, 120}[ri(2)] + ri(6)
	return p
}
