package codexreg

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/proxyutil"
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

// lookupGeoIPViaRequest 经由代理出口查询当前 IP 的地理位置，用于对齐 locale / 语言。
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
	client := &http.Client{Timeout: 12 * time.Second, Transport: transport}

	req, err := http.NewRequest(http.MethodGet,
		"http://ip-api.com/json/?fields=status,message,country,countryCode,region,city,timezone,lat,lon,query", nil)
	if err != nil {
		in.logf("⚠️ GeoIP 查询失败，跳过地理位置对齐: %v", err)
		return nil
	}
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

// localeForCountry 按国家码给出 ICU locale 与语言列表，未知国家回退 en-US。
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
