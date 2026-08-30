package higgsfieldreg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

// apiBase 是站点业务网关，pricing 页上的免费试用（绑卡优惠）接口都在这里。
const apiBase = "https://fnf-api-gw.higgsfield.ai/fnf"

const (
	pathTrialStatus = "/free-trial/status"
	pathTrialStart  = "/free-trial/start"

	// trialKindFreemium 是 pricing 页「绑卡领免费额度」用的试用类型，
	// 另一种 free_all_unlim 是订阅计划的试用，需要 plan_set_key 等额外参数。
	trialKindFreemium = "freemium_trial"
)

// apiRequest 用 Clerk 会话 JWT 访问站点业务网关。
func (c *client) apiRequest(ctx context.Context, method, path string, payload any, token string) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, apiBase+path, body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":          {"application/json, text/plain, */*"},
		"accept-language": {"en-US,en;q=0.9"},
		"authorization":   {"Bearer " + token},
		"origin":          {siteURL},
		"referer":         {siteURL + "/pricing"},
		"user-agent":      {c.ua},
	}
	if payload != nil {
		req.Header.Set("content-type", "application/json")
	}
	return c.cli.Do(req)
}

// TrialStatus 是 /free-trial/status 的返回，决定这个号能不能领绑卡优惠。
type TrialStatus struct {
	Eligible              bool   `json:"eligible"`
	FreemiumTrialEligible bool   `json:"freemium_trial_eligible"`
	UnlimTrialEligible    bool   `json:"unlim_trial_eligible"`
	Status                string `json:"status"`
	Kind                  string `json:"kind"`
	TrialCredits          any    `json:"trial_credits"`
	TrialEndsAt           string `json:"trial_ends_at"`
}

// TrialResult 绑卡优惠流程跑到「该填卡了」为止的产出。
type TrialResult struct {
	// State 流程停在哪一步：not_eligible（不符合条件）/ already_active（已在试用）/
	// need_card（收银台已开好，等真实卡信息）。
	State       string       `json:"state"`
	Kind        string       `json:"kind"`
	CheckoutURL string       `json:"checkout_url"`
	Status      *TrialStatus `json:"status"`
}

// TrialInput 绑卡优惠流程的入参。
type TrialInput struct {
	// SessionToken 注册时拿到的 Clerk 会话 JWT；过期时用 Refresh 换新的。
	SessionToken string
	Proxy        string
	Log          func(format string, a ...any)
}

// StartTrial 走 pricing 页的绑卡优惠（免费试用）流程：查资格 → 开 Stripe 收银台，
// 停在需要填真实银行卡的那一步并把收银台地址带回来。
// 本函数不提交任何卡信息：卡号由用户在收银台或后续步骤自行提供。
func StartTrial(ctx context.Context, in TrialInput) (*TrialResult, error) {
	if strings.TrimSpace(in.SessionToken) == "" {
		return nil, fmt.Errorf("缺少 Higgsfield 会话 token")
	}
	logf := func(format string, a ...any) {
		if in.Log != nil {
			in.Log(format, a...)
		}
	}
	c, err := newClient(in.Proxy)
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 客户端失败: %w", err)
	}

	status, err := c.trialStatus(ctx, in.SessionToken)
	if err != nil {
		return nil, err
	}
	logf("绑卡优惠资格：eligible=%v freemium=%v 当前状态=%q",
		status.Eligible, status.FreemiumTrialEligible, status.Status)

	switch {
	case status.Status == "active" || status.Status == "pending":
		return &TrialResult{State: "already_active", Kind: status.Kind, Status: status}, nil
	case !status.Eligible && !status.FreemiumTrialEligible:
		return &TrialResult{State: "not_eligible", Status: status}, nil
	}

	checkout, err := c.startTrial(ctx, in.SessionToken)
	if err != nil {
		return nil, err
	}
	logf("Stripe 收银台已开好，下一步需要真实银行卡信息")
	return &TrialResult{
		State:       "need_card",
		Kind:        trialKindFreemium,
		CheckoutURL: checkout,
		Status:      status,
	}, nil
}

func (c *client) trialStatus(ctx context.Context, token string) (*TrialStatus, error) {
	res, err := c.apiRequest(ctx, http.MethodGet, pathTrialStatus, nil, token)
	if err != nil {
		return nil, fmt.Errorf("查询试用资格失败: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询试用资格失败(%d): %s", res.StatusCode, trimText(string(raw), 200))
	}
	var out TrialStatus
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析试用资格失败: %w", err)
	}
	return &out, nil
}

// startTrial 请求开一个 Stripe 托管收银台，返回收银台地址（卡信息在收银台里填）。
func (c *client) startTrial(ctx context.Context, token string) (string, error) {
	payload := map[string]any{
		"kind":        trialKindFreemium,
		"success_url": siteURL + "/pricing?free_trial=success",
		"cancel_url":  siteURL + "/pricing?free_trial=cancel",
	}
	res, err := c.apiRequest(ctx, http.MethodPost, pathTrialStart, payload, token)
	if err != nil {
		return "", fmt.Errorf("开通试用失败: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("开通试用失败(%d): %s", res.StatusCode, trimText(string(raw), 200))
	}
	var out struct {
		CheckoutURL string `json:"checkout_url"`
		URL         string `json:"url"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析收银台地址失败: %w", err)
	}
	url := out.CheckoutURL
	if url == "" {
		url = out.URL
	}
	if url == "" {
		return "", fmt.Errorf("站点未返回收银台地址: %s", trimText(string(raw), 200))
	}
	return url, nil
}

// RefreshSession 用导出的 __client cookie 换一枚新的会话 JWT：JWT 只有 60 秒，
// 注册完过一阵再跑绑卡优惠时要先刷新。
func RefreshSession(ctx context.Context, proxy, clientCookie string) (string, error) {
	c, err := newClient(proxy)
	if err != nil {
		return "", err
	}
	c.setClerkCookie("__client", clientCookie)
	sessions, sessionID, err := c.clientSessions(ctx)
	if err != nil {
		return "", fmt.Errorf("读取 Clerk 客户端失败: %w", err)
	}
	if sessionID == "" {
		for _, s := range sessions {
			if s.Status == "active" {
				sessionID = s.ID
				break
			}
		}
	}
	if sessionID == "" {
		return "", fmt.Errorf("会话已失效，需要重新登录")
	}
	return c.sessionToken(ctx, sessionID)
}
