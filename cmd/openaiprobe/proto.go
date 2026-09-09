package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"
)

var probeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
var probeUAMajor = "131"

func pickProfile() profiles.ClientProfile {
	name := strings.ToLower(os.Getenv("PROBE_PROFILE"))
	if p, ok := profiles.MappedTLSClients[name]; ok {
		if i := strings.LastIndex(name, "_"); i >= 0 {
			probeUAMajor = name[i+1:]
			probeUA = strings.Replace(probeUA, "Chrome/131", "Chrome/"+probeUAMajor, 1)
		}
		fmt.Println("[proto] profile", name, "ua", probeUA)
		return p
	}
	fmt.Println("[proto] profile chrome_131")
	return profiles.Chrome_131
}

// protoProbe 不开浏览器，直接用 Chrome TLS 指纹客户端走一遍注册链路：
// providers → csrf → signin/openai → authorize（跟随跳转）→ email-verification →
// email-otp/validate（先不带 Sentinel 头，看服务端返回什么）。
func protoProbe() error {
	email := os.Getenv("PROBE_EMAIL")
	if email == "" {
		return fmt.Errorf("缺少 PROBE_EMAIL")
	}
	jar := tls_client.NewCookieJar()
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(45),
		tls_client.WithClientProfile(pickProfile()),
		tls_client.WithRandomTLSExtensionOrder(),
		tls_client.WithCookieJar(jar),
		tls_client.WithNotFollowRedirects(),
	}
	if p := os.Getenv("PROXY_URL"); p != "" {
		opts = append(opts, tls_client.WithProxyUrl(p))
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return err
	}
	deviceID := uuid.NewString()
	loggingID := uuid.NewString()

	get := func(u, referer string, doc bool) (*http.Response, string, error) {
		req, _ := http.NewRequest("GET", u, nil)
		setNavHeaders(req, referer, doc)
		resp, err := cli.Do(req)
		if err != nil {
			return nil, "", err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b), nil
	}
	show := func(tag string, resp *http.Response, body string) {
		fmt.Printf("\n=== %s → %d %s\n", tag, resp.StatusCode, resp.Header.Get("Location"))
		if sc := resp.Header.Values("Set-Cookie"); len(sc) > 0 {
			for _, c := range sc {
				fmt.Printf("  set-cookie: %s\n", trunc(c, 90))
			}
		}
		if cf := resp.Header.Get("cf-mitigated"); cf != "" {
			fmt.Printf("  cf-mitigated: %s\n", cf)
		}
		fmt.Printf("  body: %s\n", trunc(strings.ReplaceAll(body, "\n", " "), 500))
	}

	// 0. 首页（拿 chatgpt.com 的 __cf_bm / oai-did 等 Cookie）
	resp, body, err := get("https://chatgpt.com/auth/login", "", true)
	if err != nil {
		return fmt.Errorf("首页: %w", err)
	}
	show("GET chatgpt.com/auth/login", resp, body[:min(len(body), 200)])

	// 1. providers + csrf
	resp, body, err = get("https://chatgpt.com/api/auth/providers", "https://chatgpt.com/auth/login", false)
	if err != nil {
		return err
	}
	show("GET providers", resp, body)
	resp, body, err = get("https://chatgpt.com/api/auth/csrf", "https://chatgpt.com/auth/login", false)
	if err != nil {
		return err
	}
	show("GET csrf", resp, body)
	var csrf struct {
		CSRFToken string `json:"csrfToken"`
	}
	if json.Unmarshal([]byte(body), &csrf) != nil || csrf.CSRFToken == "" {
		return fmt.Errorf("拿不到 csrfToken（可能被 CF 拦截）")
	}

	// 2. signin/openai
	q := url.Values{}
	q.Set("prompt", "login")
	q.Set("ext-oai-did", deviceID)
	q.Set("auth_session_logging_id", loggingID)
	q.Set("screen_hint", "login_or_signup")
	q.Set("login_hint", email)
	form := url.Values{}
	form.Set("callbackUrl", "/")
	form.Set("csrfToken", csrf.CSRFToken)
	form.Set("json", "true")
	req, _ := http.NewRequest("POST", "https://chatgpt.com/api/auth/signin/openai?"+q.Encode(), strings.NewReader(form.Encode()))
	setNavHeaders(req, "https://chatgpt.com/auth/login", false)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "*/*")
	req.Header.Set("origin", "https://chatgpt.com")
	resp, err = cli.Do(req)
	if err != nil {
		return err
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body = string(b)
	show("POST signin/openai", resp, body)
	var signin struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(b, &signin) != nil || signin.URL == "" {
		return fmt.Errorf("signin 没返回 authorize url")
	}

	// 3. 跟随 authorize 跳转直到落在 auth.openai.com 的页面
	otpSentAt := time.Now().Add(-5 * time.Second)
	next := signin.URL
	referer := "https://chatgpt.com/"
	var landing string
	for i := 0; i < 6; i++ {
		resp, body, err = get(next, referer, true)
		if err != nil {
			return fmt.Errorf("authorize 跳转: %w", err)
		}
		show(fmt.Sprintf("GET %s", trunc(next, 80)), resp, body[:min(len(body), 300)])
		loc := resp.Header.Get("Location")
		if resp.StatusCode/100 != 3 || loc == "" {
			landing = next
			break
		}
		u, _ := url.Parse(next)
		ref, _ := u.Parse(loc)
		referer = next
		next = ref.String()
	}
	fmt.Printf("\n落地页: %s\n", landing)
	fmt.Println("auth.openai.com cookies:")
	u, _ := url.Parse("https://auth.openai.com/")
	for _, c := range cli.GetCookies(u) {
		fmt.Printf("  %s=%s\n", c.Name, trunc(c.Value, 40))
	}

	// 4. 会话快照（不需要 Sentinel）
	resp, body, err = get("https://auth.openai.com/api/accounts/client_auth_session_dump", landing, false)
	if err == nil {
		show("GET client_auth_session_dump", resp, body)
	}

	if os.Getenv("PROBE_STOP_AT_LANDING") != "" || !strings.Contains(landing, "email-verification") {
		fmt.Println("未进入验证码页，停止")
		return nil
	}

	// 5. 收验证码后不带 Sentinel 头提交
	fetch := makeCodeFetcher()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	code, err := fetch(ctx, otpSentAt)
	if err != nil {
		return fmt.Errorf("收码: %w", err)
	}
	payload, _ := json.Marshal(map[string]string{"code": code})
	req, _ = http.NewRequest("POST", "https://auth.openai.com/api/accounts/email-otp/validate", strings.NewReader(string(payload)))
	setNavHeaders(req, landing, false)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("origin", "https://auth.openai.com")
	resp, err = cli.Do(req)
	if err != nil {
		return err
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	show("POST email-otp/validate (无 Sentinel)", resp, string(b))
	var cont struct {
		ContinueURL string `json:"continue_url"`
	}
	_ = json.Unmarshal(b, &cont)
	if cont.ContinueURL == "" {
		return nil
	}
	// 6. 拉 about-you 页面并把页面引用的 JS 全部落盘，用于离线找接口
	resp, body, err = get(cont.ContinueURL, landing, true)
	if err != nil {
		return err
	}
	show("GET "+cont.ContinueURL, resp, body[:min(len(body), 300)])
	_ = os.WriteFile("about_you.html", []byte(body), 0o644)
	for _, src := range scriptSrcs(body) {
		r2, b2, err := get(src, cont.ContinueURL, false)
		if err != nil || r2.StatusCode != 200 {
			continue
		}
		name := "js_" + sanitize(src) + ".js"
		_ = os.WriteFile(name, []byte(b2), 0o644)
		fmt.Printf("  saved %s (%d bytes)\n", name, len(b2))
	}
	// 7. 提交 about-you：前端 clientAction 实际调 POST /api/accounts/create_account {name,birthdate}（带 Sentinel 头）
	age, _ := strconv.Atoi(os.Getenv("PROBE_AGE"))
	if age == 0 {
		age = 27
	}
	bd := time.Now().AddDate(-age, 0, -int(time.Now().UnixNano()%300+30)).Format("2006-01-02")
	payload, _ = json.Marshal(map[string]string{"name": os.Getenv("PROBE_NAME"), "birthdate": bd})
	req, _ = http.NewRequest("POST", "https://auth.openai.com/api/accounts/create_account", strings.NewReader(string(payload)))
	setNavHeaders(req, cont.ContinueURL, false)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set("origin", "https://auth.openai.com")
	resp, err = cli.Do(req)
	if err != nil {
		return err
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	show("POST create_account (无 Sentinel)", resp, string(b))
	var created struct {
		ContinueURL string `json:"continue_url"`
	}
	_ = json.Unmarshal(b, &created)
	if created.ContinueURL == "" {
		return nil
	}
	// continue_url 一般是 authorize 回跳 → chatgpt.com/api/auth/callback/openai?code=...
	resp, body, err = get(created.ContinueURL, cont.ContinueURL, true)
	if err != nil {
		return err
	}
	show("GET "+trunc(created.ContinueURL, 100), resp, body[:min(len(body), 300)])

	next = resp.Header.Get("Location")
	referer = created.ContinueURL
	for i := 0; i < 8 && next != "" && resp.StatusCode/100 == 3; i++ {
		u, _ := url.Parse(referer)
		ref, _ := u.Parse(next)
		next = ref.String()
		resp, body, err = get(next, referer, true)
		if err != nil {
			return fmt.Errorf("回调跳转: %w", err)
		}
		show("GET "+trunc(next, 100), resp, body[:min(len(body), 300)])
		referer = next
		next = resp.Header.Get("Location")
	}

	// 8. 读会话
	resp, body, err = get("https://chatgpt.com/api/auth/session", "https://chatgpt.com/", false)
	if err != nil {
		return err
	}
	show("GET /api/auth/session", resp, body)
	fmt.Println("chatgpt.com cookies:")
	cu, _ := url.Parse("https://chatgpt.com/")
	for _, c := range cli.GetCookies(cu) {
		fmt.Printf("  %s=%s\n", c.Name, trunc(c.Value, 30))
	}
	return nil
}

func scriptSrcs(html string) []string {
	var out []string
	for _, part := range strings.Split(html, "<script") {
		i := strings.Index(part, `src="`)
		if i < 0 || i > 200 {
			continue
		}
		rest := part[i+5:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		src := rest[:j]
		if strings.HasPrefix(src, "/") {
			src = "https://auth.openai.com" + src
		}
		if strings.Contains(src, "openai.com") {
			out = append(out, src)
		}
	}
	return out
}

func setNavHeaders(req *http.Request, referer string, doc bool) {
	req.Header.Set("user-agent", probeUA)
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("sec-ch-ua", fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24"`, probeUAMajor, probeUAMajor))
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	if referer != "" {
		req.Header.Set("referer", referer)
	}
	if doc {
		req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		req.Header.Set("upgrade-insecure-requests", "1")
		req.Header.Set("sec-fetch-site", "cross-site")
		req.Header.Set("sec-fetch-mode", "navigate")
		req.Header.Set("sec-fetch-dest", "document")
	} else {
		req.Header.Set("accept", "application/json")
		req.Header.Set("sec-fetch-site", "same-origin")
		req.Header.Set("sec-fetch-mode", "cors")
		req.Header.Set("sec-fetch-dest", "empty")
	}
	req.Header[http.HeaderOrderKey] = []string{
		"accept", "accept-language", "content-type", "origin", "referer",
		"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
		"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "upgrade-insecure-requests", "user-agent",
	}
}
