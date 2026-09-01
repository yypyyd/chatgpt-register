package luminareg

import (
	"math/rand"

	"github.com/bogdanfinn/tls-client/profiles"
)

// browserProfile 一套自洽的浏览器特征：TLS 指纹、UA、客户端提示头三者必须匹配，
// 否则组合本身就是可识别的机器人特征。每个注册任务随机取一套，避免所有请求
// 长得一模一样。
type browserProfile struct {
	clientProfile profiles.ClientProfile
	ua            string
	chUA          string
	platform      string
	lang          string
}

var browserProfiles = []browserProfile{
	{
		clientProfile: profiles.Chrome_131,
		ua:            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		chUA:          `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		platform:      `"Windows"`,
	},
	{
		clientProfile: profiles.Chrome_131_PSK,
		ua:            "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		chUA:          `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		platform:      `"macOS"`,
	},
	{
		clientProfile: profiles.Chrome_133,
		ua:            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36",
		chUA:          `"Not(A:Brand";v="99", "Google Chrome";v="133", "Chromium";v="133"`,
		platform:      `"Windows"`,
	},
	{
		clientProfile: profiles.Chrome_130_PSK,
		ua:            "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
		chUA:          `"Chromium";v="130", "Google Chrome";v="130", "Not?A_Brand";v="99"`,
		platform:      `"macOS"`,
	},
}

var acceptLanguages = []string{"en-US,en;q=0.9", "en-GB,en;q=0.9", "en-US,en;q=0.8", "en"}

func randomBrowserProfile() browserProfile {
	p := browserProfiles[rand.Intn(len(browserProfiles))]
	p.lang = acceptLanguages[rand.Intn(len(acceptLanguages))]
	return p
}
