package oreatereg

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"chatgpt-register/internal/proxyutil"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	baseURL = "https://www.oreateai.com"
	homeURL = baseURL + "/home/index/en"

	// pathSignup 只接受浏览器铸造的一次性 jt，其余 passport 接口 jt 传空即可。
	pathTicket  = "/passport/api/getticket"
	pathSignup  = "/passport/api/emailsignupin"
	pathConfirm = "/passport/api/emailregisterconfirm"
	pathLogin   = "/passport/api/emaillogin"
	pathPoints  = "/oreate/account/getpointdetail"
	// pathUserInfo 拉一次用户信息，站点会顺带把当天的每日额度（30）发到账。
	pathUserInfo = "/oreate/user/getuserinfo"
	pathRest     = "/bizapi/point/getrestpoints"
)

// client 是 Oreate 的协议客户端：注册、确认、登录、积分全部走 HTTP；
// 反爬 token jt 由浏览器铸造。
type client struct {
	cli tls_client.HttpClient
	ua  string
}

type apiStatus struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

type apiResp struct {
	Status apiStatus       `json:"status"`
	Data   json.RawMessage `json:"data"`
}

type ticket struct {
	TicketID string `json:"ticketID"`
	PK       string `json:"pk"`
}

func newClient(proxy, ua string) (*client, error) {
	if strings.TrimSpace(ua) == "" {
		ua = userAgent
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(60),
		tls_client.WithClientProfile(profiles.Chrome_131),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}
	if pu := proxyutil.Normalize(proxy); pu != "" {
		opts = append(opts, tls_client.WithProxyUrl(pu))
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, err
	}
	cli.SetFollowRedirect(true)
	return &client{cli: cli, ua: ua}, nil
}

// setCookie 覆盖站点 Cookie：HTTP 会话必须带铸造 jt 时的那个 OUID，
// 否则 jt 与设备不匹配会被站点判成参数非法。
func (c *client) setCookie(name, value string) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return
	}
	c.cli.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/", Domain: ".oreateai.com"}})
}

func (c *client) cookies() map[string]string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, ck := range c.cli.GetCookies(u) {
		out[ck.Name] = ck.Value
	}
	return out
}

func (c *client) request(ctx context.Context, method, path string, payload any, extra map[string]string) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header = http.Header{
		"accept":          {"application/json, text/plain, */*"},
		"accept-language": {"en-US,en;q=0.9"},
		"cache-control":   {"no-cache"},
		"client-type":     {"pc"},
		"content-type":    {"application/json"},
		"locale":          {"en-US"},
		"origin":          {baseURL},
		"pragma":          {"no-cache"},
		"referer":         {baseURL + "/home/index"},
		"user-agent":      {c.ua},
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return c.cli.Do(req)
}

// callAPI 发一次 JSON 请求并校验 Oreate 的业务状态码（0 = 成功）。
// 轮换代理的出口节点偶尔会直接重置连接，GET 是幂等的，遇到网络错误重试几次；
// POST（注册/确认/登录）不重试，避免重复提交。
func (c *client) callAPI(ctx context.Context, method, path string, payload any) (*apiResp, error) {
	res, err := c.request(ctx, method, path, payload, nil)
	for attempt := 1; err != nil && method == http.MethodGet && attempt < apiRetries; attempt++ {
		if !sleepCtx(ctx, apiRetryDelay) {
			return nil, err
		}
		res, err = c.request(ctx, method, path, payload, nil)
	}
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var parsed apiResp
	if err = json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%s 返回非 JSON(%d): %s", path, res.StatusCode, trimText(string(raw), 200))
	}
	if parsed.Status.Code != 0 {
		return &parsed, fmt.Errorf("%s 失败: code=%d msg=%s", path, parsed.Status.Code, parsed.Status.Msg)
	}
	return &parsed, nil
}

func (c *client) ticket(ctx context.Context) (*ticket, error) {
	resp, err := c.callAPI(ctx, http.MethodGet, pathTicket, nil)
	if err != nil {
		return nil, err
	}
	var t ticket
	if err = json.Unmarshal(resp.Data, &t); err != nil {
		return nil, err
	}
	if t.TicketID == "" || t.PK == "" {
		return nil, fmt.Errorf("获取登录票据失败：缺少 ticketID/pk")
	}
	return &t, nil
}

// encryptPassword 按站点前端一致的方式加密密码：RSA PKCS#1 v1.5 + Base64。
func encryptPassword(pubPEM, password string) (string, error) {
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		return "", fmt.Errorf("解析站点公钥失败")
	}
	// 站点下发的公钥可能是 PKCS#1，也可能是 PKIX(SPKI)，两种都兼容。
	rsaPub, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		pub, perr := x509.ParsePKIXPublicKey(block.Bytes)
		if perr != nil {
			return "", perr
		}
		var ok bool
		rsaPub, ok = pub.(*rsa.PublicKey)
		if !ok {
			return "", fmt.Errorf("站点公钥不是 RSA 公钥")
		}
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, []byte(password))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// ErrEmailTaken 该邮箱已有 Oreate 账号：emailsignupin 是「注册或登录」二合一接口，
// 邮箱已注册时它会拿本次的新密码去登录并返回密码错误，属于永久失败。
var ErrEmailTaken = errors.New("该邮箱已注册 Oreate")

// ErrSignupRejected 站点拒收这次注册（emailsignupin 返回 100002 Invalid parameter）：
// 实测是站点不收该邮箱域名（如 outlook.de），换域名的邮箱才能注册。
var ErrSignupRejected = errors.New("站点不接受该邮箱域名")

// ErrConfirmMailLimited 该邮箱的确认邮件发送次数已用完（confirmEmailStatus=2），
// 站点不会再给它发链接，只能换邮箱。
var ErrConfirmMailLimited = errors.New("确认邮件发送次数已用完")

// ErrConfirmMailTimeout 等确认邮件超时，由上层 WaitConfirmLink 包着返回：
// 确认链接 10 分钟就过期，迟到的邮件没有用，Register 收到后会重发新邮件接着等。
var ErrConfirmMailTimeout = errors.New("超时未收到 Oreate 确认邮件")

const (
	// codeWrongPassword 是站点密码错误的业务码。
	codeWrongPassword = 600005
	// codeInvalidParam 是站点参数非法的业务码。
	codeInvalidParam = 100002

	// confirmMailSent/confirmMailLimited 是站点前端的 confirmEmailStatus 枚举
	// ny={isLimited:2,sendSuccess:0}：0 为确认邮件已发出，2 为发送次数用完。
	confirmMailSent    = 0
	confirmMailLimited = 2
	// registerVerified 是站点前端的 registerStatus 枚举 _q={alreadyVerified:2}。
	registerVerified = 2

	// apiRetries/apiRetryDelay 是 GET 请求遇网络错误时的重试次数与间隔。
	apiRetries    = 3
	apiRetryDelay = 3 * time.Second
)

type signupData struct {
	IsRegister             bool `json:"isRegister"`
	SendEmailCount         int  `json:"sendEmailCount"`
	TotalCanSendEmailCount int  `json:"totalCanSendEmailCount"`
	SignupStatus           int  `json:"signupStatus"`
	SigninStatus           int  `json:"signinStatus"`
	RegisterStatus         int  `json:"registerStatus"`
	ConfirmEmailStatus     int  `json:"confirmEmailStatus"`

	// Raw 保留站点返回的原始 data，注册未受理时带进日志便于定位。
	Raw string `json:"-"`
}

// signup 提交注册，成功后站点会把确认链接发到邮箱。jt 必须是浏览器现铸的一次性 token。
func (c *client) signup(ctx context.Context, email, encPassword, ticketID, jt string) (*signupData, error) {
	resp, err := c.callAPI(ctx, http.MethodPost, pathSignup, map[string]any{
		"fr":            "main",
		"email":         email,
		"ticketID":      ticketID,
		"password":      encPassword,
		"jt":            jt,
		"utmCampaign":   "",
		"utmCampaignID": "",
		"adGroupID":     "",
		"assetGroupID":  "",
		"keywordID":     "",
		"utmTerm":       "",
	})
	if err != nil {
		if resp != nil && resp.Status.Code == codeWrongPassword {
			return nil, fmt.Errorf("%w（站点返回密码错误）", ErrEmailTaken)
		}
		if resp != nil && resp.Status.Code == codeInvalidParam {
			return nil, fmt.Errorf("%w（%s）", ErrSignupRejected, emailDomain(email))
		}
		return nil, err
	}
	var d signupData
	if err = json.Unmarshal(resp.Data, &d); err != nil {
		return nil, err
	}
	d.Raw = trimText(string(resp.Data), 300)
	return &d, nil
}

// confirm 用邮件链接里的 email + tokenID 完成注册确认，成功即同时登录（返回会话 Cookie）。
// plat 必须是字符串 "pc"，传数字会被判成参数非法。
func (c *client) confirm(ctx context.Context, email, tokenID string) error {
	_, err := c.callAPI(ctx, http.MethodPost, pathConfirm, map[string]any{
		"email":       email,
		"tokenID":     tokenID,
		"plat":        "pc",
		"fr":          "main",
		"fissionCode": "",
		"inviteCode":  "",
		"jt":          "",
	})
	return err
}

func (c *client) login(ctx context.Context, email, password string) error {
	t, err := c.ticket(ctx)
	if err != nil {
		return err
	}
	enc, err := encryptPassword(t.PK, password)
	if err != nil {
		return err
	}
	resp, err := c.callAPI(ctx, http.MethodPost, pathLogin, map[string]any{
		"fr":       "main",
		"email":    email,
		"ticketID": t.TicketID,
		"password": enc,
		"jt":       "",
	})
	if err != nil {
		return err
	}
	var d struct {
		IsLogin    bool `json:"isLogin"`
		IsVerified bool `json:"isVerified"`
	}
	if err = json.Unmarshal(resp.Data, &d); err != nil {
		return err
	}
	if !d.IsLogin {
		return fmt.Errorf("登录未生效（isVerified=%v）", d.IsVerified)
	}
	return nil
}

// touchUserInfo 拉一次用户信息：站点的每日额度（daily 30）是在首页请求用户信息时
// 自动发放的，不请求它 daily 池就一直是空的。失败不影响注册，忽略错误。
func (c *client) touchUserInfo(ctx context.Context) {
	_, _ = c.callAPI(ctx, http.MethodGet, pathUserInfo, nil)
}

// points 返回积分总额（/bizapi/point/getrestpoints 的 restPoint，站点顶栏显示的就是它）
// 与各积分池明细（daily 每日额度、bonus 赠送额度、pro 会员额度，都自动到账）。
func (c *client) points(ctx context.Context) (map[string]int, int, error) {
	resp, err := c.callAPI(ctx, http.MethodGet, pathRest, nil)
	if err != nil {
		return nil, 0, err
	}
	var rest struct {
		RestPoint int `json:"restPoint"`
	}
	if err = json.Unmarshal(resp.Data, &rest); err != nil {
		return nil, 0, err
	}
	detail := map[string]int{}
	sum := 0
	if dresp, derr := c.callAPI(ctx, http.MethodGet, pathPoints, nil); derr == nil {
		var buckets map[string]*struct {
			Amount int `json:"amount"`
		}
		if json.Unmarshal(dresp.Data, &buckets) == nil {
			for name, b := range buckets {
				// 尚未开通/未到账的池子返回 null。
				if b == nil {
					continue
				}
				detail[name] = b.Amount
				sum += b.Amount
			}
		}
	}
	// restPoint 有缓存，赠分刚到账时会比明细里的池子小，取大的那个才是真实余额。
	if sum > rest.RestPoint {
		return detail, sum, nil
	}
	return detail, rest.RestPoint, nil
}

func trimText(s string, n int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// emailDomain 取邮箱域名部分，用于提示哪个域名被站点拒收。
func emailDomain(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return email
}
