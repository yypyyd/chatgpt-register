package captcha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	yesCaptchaDefaultBase = "https://api.yescaptcha.com"
	yesCaptchaPollEvery   = 5 * time.Second
	yesCaptchaTimeout     = 4 * time.Minute
)

// YesCaptcha 封装 YesCaptcha 的 createTask/getTaskResult 接口。
// 相比 2Captcha，它对 Adobe 当前使用的 Arkose game-core 挑战支持更稳定。
type YesCaptcha struct {
	Key     string
	BaseURL string
	Client  *http.Client
	Log     func(format string, a ...any)
	// Proxy 与浏览器注册使用同一条代理；YesCaptcha 的 FunCaptcha 任务
	// 不支持 proxyless，必须把 proxyType/Address/Port/Login/Password 带上。
	Proxy string
}

func (y *YesCaptcha) logf(format string, a ...any) {
	if y.Log != nil {
		y.Log(format, a...)
	}
}

func (y *YesCaptcha) baseURL() string {
	if strings.TrimSpace(y.BaseURL) != "" {
		return strings.TrimRight(y.BaseURL, "/")
	}
	return yesCaptchaDefaultBase
}

func (y *YesCaptcha) client() *http.Client {
	if y.Client != nil {
		return y.Client
	}
	return &http.Client{Timeout: 45 * time.Second}
}

// SolveFunCaptcha 提交 Arkose FunCaptcha（Adobe 新版 game-core）任务。
// pageURL 传内层 game-core iframe 的完整 URL；surl 传 iframe 的 origin，
// 对应 YesCaptcha 的 funcaptchaApiJSSubdomain。
func (y *YesCaptcha) SolveFunCaptcha(ctx context.Context, publicKey, pageURL, surl, userAgent string) (string, error) {
	if strings.TrimSpace(y.Key) == "" {
		return "", fmt.Errorf("未配置 YesCaptcha key")
	}
	if publicKey == "" || pageURL == "" {
		return "", fmt.Errorf("缺少 Arkose publickey 或页面地址")
	}
	task := map[string]any{
		"type":                     "FunCaptchaTask",
		"websiteURL":               pageURL,
		"websitePublicKey":         publicKey,
		"funcaptchaApiJSSubdomain": surl,
	}
	if userAgent != "" {
		task["userAgent"] = userAgent
	}
	if err := addYesCaptchaProxy(task, y.Proxy); err != nil {
		return "", err
	}
	id, err := y.createTask(ctx, task)
	if err != nil {
		return "", err
	}
	y.logf("YesCaptcha 已提交 Arkose 任务 id=%s，等待结果", id)
	return y.wait(ctx, id)
}

// SolveHCaptcha 兼容原有接口；Adobe 目前主要走 Arkose，保留 hCaptcha 能力。
func (y *YesCaptcha) SolveHCaptcha(ctx context.Context, sitekey, pageURL, rqdata, userAgent string) (string, error) {
	if strings.TrimSpace(y.Key) == "" {
		return "", fmt.Errorf("未配置 YesCaptcha key")
	}
	if sitekey == "" || pageURL == "" {
		return "", fmt.Errorf("缺少 sitekey 或页面地址")
	}
	task := map[string]any{
		"type":        "HCaptchaTaskProxyless",
		"websiteURL":  pageURL,
		"websiteKey":  sitekey,
		"isInvisible": false,
	}
	if userAgent != "" {
		task["userAgent"] = userAgent
	}
	if rqdata != "" {
		task["enterprisePayload"] = map[string]any{"rqdata": rqdata}
	}
	id, err := y.createTask(ctx, task)
	if err != nil {
		return "", err
	}
	y.logf("YesCaptcha 已提交 hCaptcha 任务 id=%s，等待结果", id)
	return y.wait(ctx, id)
}

func (y *YesCaptcha) createTask(ctx context.Context, task map[string]any) (string, error) {
	var out struct {
		ErrorID          int    `json:"errorId"`
		ErrorCode        string `json:"errorCode"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           string `json:"taskId"`
	}
	if err := y.post(ctx, "/createTask", map[string]any{"clientKey": y.Key, "task": task}, &out); err != nil {
		return "", err
	}
	if out.ErrorID != 0 {
		return "", fmt.Errorf("YesCaptcha 创建任务失败: %s %s", out.ErrorCode, out.ErrorDescription)
	}
	if out.TaskID == "" {
		return "", fmt.Errorf("YesCaptcha 未返回 taskId")
	}
	return out.TaskID, nil
}

func (y *YesCaptcha) wait(ctx context.Context, id string) (string, error) {
	deadline := time.Now().Add(yesCaptchaTimeout)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("YesCaptcha 打码超时")
		}
		var out struct {
			ErrorID          int    `json:"errorId"`
			ErrorCode        string `json:"errorCode"`
			ErrorDescription string `json:"errorDescription"`
			Status           string `json:"status"`
			Solution         struct {
				Token string `json:"token"`
			} `json:"solution"`
		}
		if err := y.post(ctx, "/getTaskResult", map[string]any{"clientKey": y.Key, "taskId": id}, &out); err != nil {
			return "", err
		}
		if out.ErrorID != 0 {
			return "", fmt.Errorf("YesCaptcha 打码失败: %s %s", out.ErrorCode, out.ErrorDescription)
		}
		if out.Status == "ready" {
			if strings.TrimSpace(out.Solution.Token) == "" {
				return "", fmt.Errorf("YesCaptcha 返回空 token")
			}
			return out.Solution.Token, nil
		}
		if !sleepCtx(ctx, yesCaptchaPollEvery) {
			return "", ctx.Err()
		}
	}
}

func (y *YesCaptcha) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, y.baseURL()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := y.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解析 YesCaptcha 响应失败: %w", err)
	}
	return nil
}

// addYesCaptchaProxy 把 http(s)/socks5 代理拆成 YesCaptcha FunCaptchaTask 需要的字段。
func addYesCaptchaProxy(task map[string]any, proxy string) error {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return fmt.Errorf("YesCaptcha FunCaptcha 需要注册代理，但当前任务没有可用代理")
	}
	raw := proxy
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("解析 YesCaptcha 代理失败")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return fmt.Errorf("解析 YesCaptcha 代理端口失败")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	case "socks4":
		scheme = "socks4"
	default:
		scheme = "http"
	}
	task["proxyType"] = scheme
	task["proxyAddress"] = u.Hostname()
	task["proxyPort"] = port
	if u.User != nil {
		task["proxyLogin"] = u.User.Username()
		if pass, ok := u.User.Password(); ok {
			task["proxyPassword"] = pass
		}
	}
	return nil
}
