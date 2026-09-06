package codexreg

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/proxyutil"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// geoInfo 是 ip-api.com 的地理定位结果。
type geoInfo struct {
	Status      string  `json:"status"`
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	Region      string  `json:"region"`
	City        string  `json:"city"`
	Timezone    string  `json:"timezone"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	Query       string  `json:"query"`
}

// lookupGeoIPViaRequest 直接发起 HTTP 请求（经由代理出口）查询当前出口 IP 的地理位置，
// 不占用浏览器页面，从而可在创建页面前拿到地理信息、一次性注入一致指纹。
func lookupGeoIPViaRequest(in Input) *geoInfo {
	in.logf("🌍 正在通过代理查询出口 IP 地理位置...")

	transport := &http.Transport{}
	if strings.TrimSpace(in.Proxy) != "" {
		pu, perr := url.Parse(proxyutil.Normalize(in.Proxy))
		if perr != nil {
			in.logf("⚠️ 代理解析失败，跳过地理位置对齐: %v", perr)
			return nil
		}
		transport.Proxy = http.ProxyURL(pu)
	}
	// 只是拿地区做语言/时区对齐，拿不到就回退 en-US；别让它吃掉注册预算。
	client := &http.Client{Timeout: 12 * time.Second, Transport: transport}

	req, err := http.NewRequest(http.MethodGet,
		"http://ip-api.com/json/?fields=status,message,country,countryCode,region,city,timezone,lat,lon,query", nil)
	if err != nil {
		in.logf("⚠️ GeoIP 查询失败，跳过地理位置对齐: %v", err)
		return nil
	}
	// 带上正常浏览器的 UA/语言，避免被 ip-api 以空 UA 拒绝
	req.Header.Set("User-Agent", geoLookupUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		in.logf("⚠️ GeoIP 查询失败，跳过地理位置对齐: %v", err)
		return nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var g geoInfo
	if err := json.Unmarshal(body, &g); err != nil || g.Status != "success" {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		in.logf("⚠️ GeoIP 查询失败，跳过地理位置对齐 (HTTP %d, resp=%q)", resp.StatusCode, snippet)
		return nil
	}
	in.logf("📍 出口 IP=%s 位置=%s/%s 时区=%s (%.4f, %.4f)",
		g.Query, g.CountryCode, g.City, g.Timezone, g.Lat, g.Lon)
	return &g
}

// applyGeo 把地理信息映射到浏览器：时区、经纬度、locale。
// UA / Accept-Language 已在 Session.NewPage 里按同一份地理信息注入。
func applyGeo(page *rod.Page, g *geoInfo, in Input) {
	if g.Timezone != "" {
		_ = (proto.EmulationSetTimezoneOverride{TimezoneID: g.Timezone}).Call(page)
	}
	lat, lon, acc := g.Lat, g.Lon, 50.0
	_ = (proto.EmulationSetGeolocationOverride{Latitude: &lat, Longitude: &lon, Accuracy: &acc}).Call(page)

	locale, languages := localeForCountry(g.CountryCode)
	_ = (proto.EmulationSetLocaleOverride{Locale: locale}).Call(page)
	in.logf("✅ 已对齐时区/坐标/语言: tz=%s locale=%s lang=%s", g.Timezone, locale, languages)
}

// localeForCountry 按国家码给出 ICU locale 与语言列表，未知国家回退 en-US。
// 语言列表不带 q 值：CDP 的 acceptLanguage 只接受语言标签列表，Chrome 自己生成
// Accept-Language 的 q 值与 navigator.languages；带 q 值传进去会得到
// "en-US,en;q=0.9;q=0.9" 这种畸形请求头。
func localeForCountry(cc string) (locale, languages string) {
	switch strings.ToUpper(strings.TrimSpace(cc)) {
	case "US":
		return "en_US", "en-US,en"
	case "GB", "UK":
		return "en_GB", "en-GB,en"
	case "CA":
		return "en_CA", "en-CA,en,fr-CA"
	case "AU":
		return "en_AU", "en-AU,en"
	case "DE":
		return "de_DE", "de-DE,de,en"
	case "FR":
		return "fr_FR", "fr-FR,fr,en"
	case "ES":
		return "es_ES", "es-ES,es,en"
	case "IT":
		return "it_IT", "it-IT,it,en"
	case "NL":
		return "nl_NL", "nl-NL,nl,en"
	case "JP":
		return "ja_JP", "ja-JP,ja,en"
	case "KR":
		return "ko_KR", "ko-KR,ko,en"
	case "BR":
		return "pt_BR", "pt-BR,pt,en"
	case "RU":
		return "ru_RU", "ru-RU,ru,en"
	case "IN":
		return "en_IN", "en-IN,en,hi"
	case "SG":
		return "en_SG", "en-SG,en"
	default:
		return "en_US", "en-US,en"
	}
}
