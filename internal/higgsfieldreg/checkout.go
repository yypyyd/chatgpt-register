package higgsfieldreg

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

// Stripe 托管收银台（checkout.stripe.com）背后是一组公开 key 就能调的接口，
// 绑卡这步不需要真的开浏览器：init 拿页面上下文 → payment_methods 换 pm_ →
// payment_pages/{cs}/confirm 提交。confirm 之后 Stripe 可能要求先过一次
// hCaptcha 挑战（intent_confirmation_challenge），过了才会把 SetupIntent 置为
// succeeded、订阅才会进 trialing。
const (
	stripeAPIBase   = "https://api.stripe.com"
	stripeCheckout  = "https://checkout.stripe.com"
	stripePaymentUA = "stripe.js/30af02f6fc; stripe-js-v3/30af02f6fc; checkout"

	// challengePageURL 是 Stripe 承载 hCaptcha 的页面域，打码时必须按它报 host，
	// 否则拿回来的 token 和挑战对不上。
	challengePageURL = "https://b.stripecdn.com"

	pathWorkspaceSub = "/workspaces/subscription"
)

// 绑卡流程停在哪一步。
const (
	// BindStateSucceeded SetupIntent 已 succeeded，卡绑上了。
	BindStateSucceeded = "succeeded"
	// BindStateNeedChallenge Stripe 要求过 hCaptcha，但没提供打码回调。
	BindStateNeedChallenge = "need_challenge"
	// BindStateChallengeFailed 打码 token 被 Stripe 拒绝（挑战没过）。
	BindStateChallengeFailed = "challenge_failed"
	// BindStateDeclined 卡被拒 / 认证失败等 Stripe 侧的支付错误。
	BindStateDeclined = "declined"
)

var pkPattern = regexp.MustCompile(`pk_live_[A-Za-z0-9]+`)

// Card 是要绑到收银台上的真实银行卡与账单地址。程序不生成也不校验卡信息，
// 全部由调用方提供。
type Card struct {
	Number   string
	ExpMonth string
	ExpYear  string
	CVC      string

	Name       string
	Country    string
	State      string
	City       string
	Line1      string
	PostalCode string
}

// Challenge 是 confirm 之后 Stripe 下发的 hCaptcha 挑战参数。
type Challenge struct {
	SiteKey string
	// RqData 是 enterprise hCaptcha 的绑定数据，打码时必须原样带上。
	RqData string
	// PageURL 挑战所在页面（Stripe 固定用 b.stripecdn.com）。
	PageURL string
	// VerificationURL 解出 token 后回交给 Stripe 的地址（相对 api.stripe.com）。
	VerificationURL string
	ClientSecret    string
	UserAgent       string
}

// ChallengeSolution 打码平台产出的挑战结果，ekey 有就一起回交。
type ChallengeSolution struct {
	Token string
	EKey  string
}

// BindCardInput 绑卡流程的入参。
type BindCardInput struct {
	// CheckoutURL 是 StartTrial 返回的收银台地址。
	CheckoutURL string
	Card        Card
	// SessionToken 用来在绑完卡后回查 Higgsfield 的试用/订阅状态，可留空。
	SessionToken string
	Proxy        string
	// SolveChallenge 在真实挑战里产出 token（打码平台或人工），
	// 留空时流程遇到挑战就停下来并把参数带回给调用方，不做任何伪造。
	SolveChallenge func(ctx context.Context, ch Challenge) (ChallengeSolution, error)
	Log            func(format string, a ...any)
}

// BindCardResult 绑卡流程的产出。
type BindCardResult struct {
	State             string `json:"state"`
	CheckoutSessionID string `json:"checkout_session_id"`
	PaymentMethodID   string `json:"payment_method_id"`
	SetupIntentID     string `json:"setup_intent_id"`
	SetupIntentStatus string `json:"setup_intent_status"`
	CheckoutStatus    string `json:"checkout_status"`
	PaymentStatus     string `json:"payment_status"`
	// StripeError 是 Stripe 给出的失败原因（last_setup_error 或 confirm 的报错）。
	StripeError string `json:"stripe_error"`
	// Challenge 在 State 为 need_challenge / challenge_failed 时带回挑战参数。
	Challenge *Challenge `json:"-"`
	// Trial / Subscription 是绑卡后回查到的 Higgsfield 侧状态。
	Trial        *TrialStatus  `json:"trial"`
	Subscription *Subscription `json:"subscription"`
}

// Subscription 是 /workspaces/subscription 的返回，判断试用有没有真的生效。
type Subscription struct {
	PlanName string `json:"plan_name"`
	PlanType string `json:"plan_type"`
	Status   string `json:"status"`
}

// ParseCheckoutURL 从收银台地址里取出会话 id 与页面用的 publishable key：
// key 藏在 URL fragment 里，是 base64 编码后逐字节异或 5 的页面初始数据。
func ParseCheckoutURL(checkoutURL string) (sessionID, publishableKey string, err error) {
	rest := checkoutURL
	if i := strings.Index(rest, "/pay/"); i >= 0 {
		rest = rest[i+len("/pay/"):]
	} else {
		return "", "", fmt.Errorf("不是 Stripe 收银台地址: %s", trimText(checkoutURL, 80))
	}
	frag := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		sessionID, frag = rest[:i], rest[i+1:]
	} else {
		sessionID = rest
	}
	if sessionID == "" {
		return "", "", fmt.Errorf("收银台地址里没有会话 id")
	}
	if frag == "" {
		return sessionID, "", fmt.Errorf("收银台地址里没有页面数据")
	}
	decoded, err := url.QueryUnescape(frag)
	if err != nil {
		decoded = frag
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(decoded, "="))
	if err != nil {
		return sessionID, "", fmt.Errorf("解析收银台页面数据失败: %w", err)
	}
	for i := range raw {
		raw[i] ^= 5
	}
	key := pkPattern.FindString(string(raw))
	if key == "" {
		return sessionID, "", fmt.Errorf("收银台页面数据里没有 publishable key")
	}
	return sessionID, key, nil
}

// BindCard 在 StartTrial 开出的收银台上提交真实银行卡：
// init → payment_methods → confirm →（如有）过 hCaptcha 挑战 → 回查状态。
func BindCard(ctx context.Context, in BindCardInput) (*BindCardResult, error) {
	logf := func(format string, a ...any) {
		if in.Log != nil {
			in.Log(format, a...)
		}
	}
	sessionID, pk, err := ParseCheckoutURL(in.CheckoutURL)
	if err != nil {
		return nil, err
	}
	c, err := newClient(in.Proxy)
	if err != nil {
		return nil, fmt.Errorf("创建 HTTP 客户端失败: %w", err)
	}
	out := &BindCardResult{CheckoutSessionID: sessionID}

	// init 让 Stripe 认为页面已经打开过：不先 init 直接 confirm 会被判成非法调用面。
	if err = c.checkoutInit(ctx, sessionID, pk); err != nil {
		return nil, err
	}
	logf("收银台已初始化：%s", sessionID)

	pmID, err := c.createPaymentMethod(ctx, pk, in.Card)
	if err != nil {
		return nil, err
	}
	out.PaymentMethodID = pmID
	logf("卡已换成支付方式 %s", pmID)

	if err = c.checkoutConfirm(ctx, sessionID, pk, pmID); err != nil {
		out.State = BindStateDeclined
		out.StripeError = err.Error()
		return out, nil
	}

	state, err := c.checkoutState(ctx, sessionID, pk)
	if err != nil {
		return nil, err
	}
	applyState(out, state)

	if ch := state.challenge(); ch != nil {
		ch.UserAgent = c.ua
		out.Challenge = ch
		if in.SolveChallenge == nil {
			out.State = BindStateNeedChallenge
			logf("Stripe 要求过 hCaptcha 挑战，但没有配置打码回调")
			c.attachSiteState(ctx, out, in.SessionToken)
			return out, nil
		}
		logf("Stripe 下发 hCaptcha 挑战，开始打码：sitekey=%s", ch.SiteKey)
		sol, serr := in.SolveChallenge(ctx, *ch)
		if serr != nil {
			out.State = BindStateChallengeFailed
			out.StripeError = serr.Error()
			c.attachSiteState(ctx, out, in.SessionToken)
			return out, nil
		}
		if err = c.verifyChallenge(ctx, pk, *ch, sol); err != nil {
			out.State = BindStateChallengeFailed
			out.StripeError = err.Error()
			c.attachSiteState(ctx, out, in.SessionToken)
			return out, nil
		}
		if state, err = c.waitCheckout(ctx, sessionID, pk); err != nil {
			return nil, err
		}
		applyState(out, state)
	}

	switch out.SetupIntentStatus {
	case "succeeded":
		out.State = BindStateSucceeded
	case "requires_action":
		out.State = BindStateNeedChallenge
	default:
		out.State = BindStateDeclined
		if out.Challenge != nil {
			out.State = BindStateChallengeFailed
		}
	}
	logf("绑卡结果：%s（SetupIntent=%s）", out.State, out.SetupIntentStatus)
	c.attachSiteState(ctx, out, in.SessionToken)
	return out, nil
}

// checkoutStateData 是 payment_pages/{cs} 里流程要看的字段。
type checkoutStateData struct {
	Status        string `json:"status"`
	PaymentStatus string `json:"payment_status"`
	SetupIntent   struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		ClientSecret   string `json:"client_secret"`
		LastSetupError *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"last_setup_error"`
		NextAction *struct {
			Type         string `json:"type"`
			UseStripeSDK *struct {
				StripeJS *struct {
					SiteKey         string `json:"site_key"`
					RqData          string `json:"rqdata"`
					VerificationURL string `json:"verification_url"`
				} `json:"stripe_js"`
			} `json:"use_stripe_sdk"`
		} `json:"next_action"`
	} `json:"setup_intent"`
}

// challenge 把 next_action 里的 hCaptcha 参数取出来，没有挑战时返回 nil。
func (s *checkoutStateData) challenge() *Challenge {
	na := s.SetupIntent.NextAction
	if na == nil || na.UseStripeSDK == nil || na.UseStripeSDK.StripeJS == nil {
		return nil
	}
	js := na.UseStripeSDK.StripeJS
	if js.SiteKey == "" || js.VerificationURL == "" {
		return nil
	}
	return &Challenge{
		SiteKey:         js.SiteKey,
		RqData:          js.RqData,
		PageURL:         challengePageURL,
		VerificationURL: js.VerificationURL,
		ClientSecret:    s.SetupIntent.ClientSecret,
	}
}

func applyState(out *BindCardResult, s *checkoutStateData) {
	out.CheckoutStatus = s.Status
	out.PaymentStatus = s.PaymentStatus
	out.SetupIntentID = s.SetupIntent.ID
	out.SetupIntentStatus = s.SetupIntent.Status
	if e := s.SetupIntent.LastSetupError; e != nil {
		out.StripeError = strings.TrimSpace(e.Code + " " + e.Message)
	}
}

// stripeForm 发一次 Stripe 表单请求，非 2xx 或带 error 字段时转成错误。
func (c *client) stripeForm(ctx context.Context, path string, form url.Values) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodPost, stripeAPIBase+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":          {"application/json"},
		"content-type":    {"application/x-www-form-urlencoded"},
		"origin":          {stripeCheckout},
		"referer":         {stripeCheckout + "/"},
		"user-agent":      {c.ua},
		"accept-language": {"en-US,en;q=0.9"},
	}
	res, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &wrapped)
	if wrapped.Error != nil {
		return nil, fmt.Errorf("Stripe %s 失败 %s: %s", path, wrapped.Error.Code, trimText(wrapped.Error.Message, 200))
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("Stripe %s 返回 %d: %s", path, res.StatusCode, trimText(string(raw), 200))
	}
	return raw, nil
}

func (c *client) checkoutInit(ctx context.Context, sessionID, pk string) error {
	_, err := c.stripeForm(ctx, "/v1/payment_pages/"+sessionID+"/init", url.Values{
		"key":            {pk},
		"eid":            {"NA"},
		"browser_locale": {"en-US"},
		"redirect_type":  {"stripe_js"},
	})
	return err
}

// createPaymentMethod 用页面 publishable key 把卡换成 pm_ 标识。
func (c *client) createPaymentMethod(ctx context.Context, pk string, card Card) (string, error) {
	form := url.Values{
		"key":                                   {pk},
		"payment_user_agent":                    {stripePaymentUA},
		"type":                                  {"card"},
		"card[number]":                          {strings.ReplaceAll(card.Number, " ", "")},
		"card[exp_month]":                       {card.ExpMonth},
		"card[exp_year]":                        {card.ExpYear},
		"card[cvc]":                             {card.CVC},
		"billing_details[name]":                 {card.Name},
		"billing_details[address][country]":     {card.Country},
		"billing_details[address][state]":       {card.State},
		"billing_details[address][city]":        {card.City},
		"billing_details[address][line1]":       {card.Line1},
		"billing_details[address][postal_code]": {card.PostalCode},
	}
	raw, err := c.stripeForm(ctx, "/v1/payment_methods", form)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("Stripe 未返回支付方式: %s", trimText(string(raw), 200))
	}
	return out.ID, nil
}

// checkoutConfirm 提交收银台，$0 试用的 expected_amount 固定为 0。
func (c *client) checkoutConfirm(ctx context.Context, sessionID, pk, pmID string) error {
	_, err := c.stripeForm(ctx, "/v1/payment_pages/"+sessionID+"/confirm", url.Values{
		"key":             {pk},
		"expected_amount": {"0"},
		"payment_method":  {pmID},
	})
	return err
}

func (c *client) checkoutState(ctx context.Context, sessionID, pk string) (*checkoutStateData, error) {
	req, err := http.NewRequest(http.MethodGet,
		stripeAPIBase+"/v1/payment_pages/"+sessionID+"?"+url.Values{"key": {pk}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":     {"application/json"},
		"origin":     {stripeCheckout},
		"referer":    {stripeCheckout + "/"},
		"user-agent": {c.ua},
	}
	res, err := c.cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询收银台状态失败: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询收银台状态失败(%d): %s", res.StatusCode, trimText(string(raw), 200))
	}
	var out checkoutStateData
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析收银台状态失败: %w", err)
	}
	return &out, nil
}

// waitCheckout 过完挑战后等 SetupIntent 落定（Stripe 侧是异步的）。
func (c *client) waitCheckout(ctx context.Context, sessionID, pk string) (*checkoutStateData, error) {
	var last *checkoutStateData
	for i := 0; i < 8; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
		state, err := c.checkoutState(ctx, sessionID, pk)
		if err != nil {
			return nil, err
		}
		last = state
		switch state.SetupIntent.Status {
		case "succeeded", "canceled", "requires_payment_method":
			return state, nil
		}
	}
	return last, nil
}

// verifyChallenge 把打码拿到的 token 回交给 Stripe，过了挑战才会继续认证。
func (c *client) verifyChallenge(ctx context.Context, pk string, ch Challenge, sol ChallengeSolution) error {
	if strings.TrimSpace(sol.Token) == "" {
		return fmt.Errorf("打码没有返回 token")
	}
	form := url.Values{
		"key":                      {pk},
		"challenge_response_token": {sol.Token},
		"challenge_response_ekey":  {sol.EKey},
	}
	if ch.ClientSecret != "" {
		form.Set("client_secret", ch.ClientSecret)
	}
	_, err := c.stripeForm(ctx, ch.VerificationURL, form)
	return err
}

// attachSiteState 回查 Higgsfield 侧的试用与订阅状态：只有这里变了才算真的开通。
func (c *client) attachSiteState(ctx context.Context, out *BindCardResult, token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	if status, err := c.trialStatus(ctx, token); err == nil {
		out.Trial = status
	}
	if sub, err := c.subscription(ctx, token); err == nil {
		out.Subscription = sub
	}
}

func (c *client) subscription(ctx context.Context, token string) (*Subscription, error) {
	res, err := c.apiRequest(ctx, http.MethodGet, pathWorkspaceSub, nil, token)
	if err != nil {
		return nil, fmt.Errorf("查询订阅失败: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询订阅失败(%d): %s", res.StatusCode, trimText(string(raw), 200))
	}
	var out Subscription
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析订阅失败: %w", err)
	}
	return &out, nil
}
