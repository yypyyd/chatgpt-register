package codexreg

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	fallbackChromeMajor = "146"
	fallbackChromeFull  = "146.0.7680.164"
	fallbackTLSProfile  = "chrome_146_PSK"
)

var (
	chromeNameRe = regexp.MustCompile(`(?i)(?:google\s+)?chrom(?:e|ium)[\s/]+(\d+)`)
	chromeVerRe  = regexp.MustCompile(`\b(\d{2,3})\.\d+\.\d+\.\d+`)
	chromeUARe   = regexp.MustCompile(`Chrome/(\d+)`)
)

func parseChromeMajor(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ""
	}
	for _, re := range []*regexp.Regexp{chromeNameRe, chromeVerRe} {
		if m := re.FindStringSubmatch(version); len(m) == 2 {
			n, err := strconv.Atoi(m[1])
			if err == nil && n >= 100 && n < 300 {
				return m[1]
			}
		}
	}
	return ""
}

func chromeMajorFromUA(ua string) string {
	if m := chromeUARe.FindStringSubmatch(ua); len(m) == 2 {
		return m[1]
	}
	return ""
}

func chromeFullVersion(major string) string {
	major = strings.TrimSpace(major)
	if major == "" || major == fallbackChromeMajor {
		return fallbackChromeFull
	}
	return major + ".0.0.0"
}

func chromeUserAgent(major string) string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromeFullVersion(major) + " Safari/537.36"
}

// Chrome 的 sec-ch-ua 品牌列表带一个“GREASE”品牌，品牌名、版本号与三项顺序都由主版本号决定
// （components/embedder_support/user_agent_utils.cc）。用真实 Chrome 154 的
// `"Chromium";v="154", "Google Chrome";v="154", "Not A(Brand";v="99"` 逐项核对过。
// 固定写成 "Not_A Brand";v="8" 只对老版本成立，与声称的 UA 版本不一致会被一眼看出。
var (
	greaseChars  = []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	greaseVers   = []string{"8", "99", "24"}
	greaseOrders = [6][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
)

// chromeBrandList 返回按 Chrome 规则排好序的 (品牌, 版本) 三元组；fullVersion 非空时用于完整版本列表。
func chromeBrandList(major int, fullVersion string) [3][2]string {
	if major <= 0 {
		major = 146
	}
	greaseBrand := "Not" + greaseChars[major%len(greaseChars)] + "A" + greaseChars[(major+1)%len(greaseChars)] + "Brand"
	greaseVer := greaseVers[major%len(greaseVers)]
	ver := strconv.Itoa(major)
	if fullVersion != "" {
		greaseVer += ".0.0.0"
		ver = fullVersion
	}
	order := greaseOrders[major%6]
	var out [3][2]string
	out[order[0]] = [2]string{greaseBrand, greaseVer}
	out[order[1]] = [2]string{"Chromium", ver}
	out[order[2]] = [2]string{"Google Chrome", ver}
	return out
}

func joinBrandList(list [3][2]string) string {
	parts := make([]string, 0, 3)
	for _, b := range list {
		parts = append(parts, fmt.Sprintf(`"%s";v="%s"`, b[0], b[1]))
	}
	return strings.Join(parts, ", ")
}

func secCHUA(major string) string {
	if strings.TrimSpace(major) == "" {
		major = fallbackChromeMajor
	}
	n, _ := strconv.Atoi(strings.TrimSpace(major))
	return joinBrandList(chromeBrandList(n, ""))
}

func protocolTLSProfileName() string {
	return fallbackTLSProfile
}

func primaryLanguageTag(languages string) string {
	p := strings.TrimSpace(strings.Split(languages, ",")[0])
	if p == "" {
		return "en-US"
	}
	return strings.ReplaceAll(p, "_", "-")
}

func bcp47Locale(locale string) string {
	locale = strings.TrimSpace(locale)
	if locale == "" {
		return "en-US"
	}
	return strings.ReplaceAll(locale, "_", "-")
}

// DeviceIDFromCookies 取出注册时 chatgpt.com 下发的稳定设备 ID。
func DeviceIDFromCookies(cookies []WebCookie) string {
	for _, name := range []string{"oai-did", "oai-device-id"} {
		if v := cookieNamed(cookies, name); v != "" {
			return v
		}
	}
	return ""
}

func cookieNamed(cookies []WebCookie, name string) string {
	for _, c := range cookies {
		if strings.EqualFold(strings.TrimSpace(c.Name), name) && strings.TrimSpace(c.Value) != "" {
			return c.Value
		}
	}
	return ""
}
