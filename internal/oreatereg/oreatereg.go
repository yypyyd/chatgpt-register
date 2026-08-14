// Package oreatereg 完成 oreateai.com 的全协议注册：注册 → 邮件确认链接 →
// 登录 → 校验自动到账的积分 → 用 Kling3.0 Omini 1k 生成一张图（再自动加赠积分），
// 并采集站点会话 Cookie 供 2api 导出。
// 只有反爬 token jt 必须由浏览器现铸（一次性、与 OUID 绑定），其余全部走 HTTP。
package oreatereg

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

const (
	// 图二里的模型档位：Kling3.0 Omini / 1k / 1:1，单次 6 积分。
	imageModel      = "Kling3.0 Omini"
	imageResolution = "1k"
	imageRatio      = "1:1"

	defaultPrompt = "a cute orange cat sitting on a wooden desk, soft light"

	// pointsWaitTimeout 等注册赠分异步到账的最长时间。
	pointsWaitTimeout = 60 * time.Second
)

// confirmLinkRe 匹配注册确认邮件里的链接：/home/index?email=...&tokenID=...
var confirmLinkRe = regexp.MustCompile(`https://[\w.\-]*oreateai\.com/[^\s"'<>]*tokenID=[0-9a-zA-Z\-]+`)

type Input struct {
	Email    string
	Password string
	Proxy    string
	Headless bool

	// WaitConfirmLink 返回注册确认邮件里的链接（由上层从邮箱轮询取回）。
	WaitConfirmLink func(ctx context.Context) (string, error)
	Log             func(format string, a ...any)
}

// Result 是注册产出：会话 Cookie（含 ouss 会话票据）、积分与出图地址。
type Result struct {
	Email       string            `json:"email"`
	Password    string            `json:"password"`
	Cookies     map[string]string `json:"cookies"`
	OUID        string            `json:"ouid"`
	UserAgent   string            `json:"user_agent"`
	PointDetail map[string]int    `json:"point_detail"`
	Points      int               `json:"points"`
	ImageURL    string            `json:"image_url"`
	ImageModel  string            `json:"image_model"`
	CapturedAt  string            `json:"captured_at"`
}

func (in Input) logf(format string, a ...any) {
	if in.Log != nil {
		in.Log(format, a...)
	}
}

// Register 跑完整套注册流程并返回可导出的会话数据。
func Register(ctx context.Context, in Input) (*Result, error) {
	if in.Email == "" {
		return nil, fmt.Errorf("缺少邮箱")
	}
	if in.WaitConfirmLink == nil {
		return nil, fmt.Errorf("缺少确认链接回调")
	}
	if in.Password == "" {
		in.Password = GenPassword(16)
	}

	// 注册与生图各要一个一次性 jt，一次浏览器会话铸两个，省一次启动。
	in.logf("正在用浏览器铸造反爬 token（注册与生图各一个）")
	tk, err := mintTokens(ctx, in, 2)
	if err != nil {
		return nil, err
	}

	cli, err := newClient(in.Proxy, tk.UA)
	if err != nil {
		return nil, err
	}
	// jt 与铸造它的设备绑定，HTTP 会话必须带同一个 OUID。
	cli.setCookie("OUID", tk.OUID)

	t, err := cli.ticket(ctx)
	if err != nil {
		return nil, err
	}
	encPassword, err := encryptPassword(t.PK, in.Password)
	if err != nil {
		return nil, err
	}
	signup, err := cli.signup(ctx, in.Email, encPassword, t.TicketID, tk.JTs[0])
	if err != nil {
		return nil, err
	}
	if !signup.IsRegister {
		return nil, fmt.Errorf("站点未受理注册（signupStatus=%d）", signup.SignupStatus)
	}
	in.logf("注册已提交，确认邮件已发出，等待邮箱里的确认链接")

	link, err := in.WaitConfirmLink(ctx)
	if err != nil {
		return nil, err
	}
	linkEmail, tokenID, err := parseConfirmLink(link)
	if err != nil {
		return nil, err
	}
	if linkEmail == "" {
		linkEmail = in.Email
	}
	if err = cli.confirm(ctx, linkEmail, tokenID); err != nil {
		return nil, err
	}
	in.logf("确认链接已生效，账号已激活")

	// 用密码登录一次，确认导出的账号密码可用并拿到干净的会话票据。
	if err = cli.login(ctx, in.Email, in.Password); err != nil {
		return nil, err
	}
	cookies := cli.cookies()
	if cookies["ouss"] == "" {
		return nil, fmt.Errorf("登录成功但没拿到会话票据 ouss")
	}
	in.logf("登录成功，已拿到站点会话")

	// 注册赠分是异步到账的，等它到账再生图，否则会因积分不足出图失败。
	detail, total, err := waitPoints(ctx, cli)
	if err != nil {
		return nil, err
	}
	in.logf("注册自动到账积分: %d（%s）", total, formatPoints(detail))

	// 生图前把 OUID/UA 换回铸造 jt 的那套，否则会被判成 spam user。
	cli.setCookie("OUID", tk.OUID)
	cli.ua = tk.UA
	imageURL, genErr := generate(ctx, cli, in, tk)
	if genErr != nil {
		in.logf("生图失败: %v（账号已注册可用，积分加赠未完成）", genErr)
	}

	detail, total, err = cli.points(ctx)
	if err != nil {
		return nil, err
	}
	in.logf("生图后积分: %d（%s）", total, formatPoints(detail))

	cookies = cli.cookies()
	cookies["OUID"] = tk.OUID
	return &Result{
		Email:       in.Email,
		Password:    in.Password,
		Cookies:     cookies,
		OUID:        tk.OUID,
		UserAgent:   tk.UA,
		PointDetail: detail,
		Points:      total,
		ImageURL:    imageURL,
		ImageModel:  imageModel + " " + imageResolution,
		CapturedAt:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// generate 建会话并出一张图，出图后站点自动加赠积分。
func generate(ctx context.Context, cli *client, in Input, tk *tokens) (string, error) {
	chatID, err := cli.createChat(ctx)
	if err != nil {
		return "", err
	}
	in.logf("已创建生图会话，调用 %s %s（%s）", imageModel, imageResolution, imageRatio)
	jt := tk.JTs[0]
	if len(tk.JTs) > 1 {
		jt = tk.JTs[1]
	}
	imageURL, err := cli.generateImage(ctx, chatID, defaultPrompt, jt, tk.OUID, in.Email)
	if err != nil {
		return imageURL, err
	}
	in.logf("生图完成: %s", imageURL)
	return imageURL, nil
}

// waitPoints 轮询积分，等注册赠分到账（最多等 pointsWaitTimeout）。
func waitPoints(ctx context.Context, cli *client) (map[string]int, int, error) {
	deadline := time.Now().Add(pointsWaitTimeout)
	for {
		detail, total, err := cli.points(ctx)
		if err != nil {
			return nil, 0, err
		}
		if total > 0 || time.Now().After(deadline) {
			return detail, total, nil
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return detail, total, ctx.Err()
		}
	}
}

// parseConfirmLink 从确认链接里取出 email 与 tokenID。
func parseConfirmLink(link string) (string, string, error) {
	link = strings.TrimSpace(link)
	u, err := url.Parse(link)
	if err != nil {
		return "", "", fmt.Errorf("解析确认链接失败: %w", err)
	}
	tokenID := u.Query().Get("tokenID")
	if tokenID == "" {
		return "", "", fmt.Errorf("确认链接里没有 tokenID")
	}
	return u.Query().Get("email"), tokenID, nil
}

// ExtractConfirmLink 从邮件正文（HTML 或纯文本）里取出注册确认链接。
func ExtractConfirmLink(body string) string {
	link := confirmLinkRe.FindString(strings.ReplaceAll(body, "&amp;", "&"))
	return strings.TrimRight(link, `.,)"'`)
}

func formatPoints(detail map[string]int) string {
	parts := make([]string, 0, len(detail))
	for _, name := range []string{"daily", "bonus"} {
		if v, ok := detail[name]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", name, v))
		}
	}
	for name, v := range detail {
		if name == "daily" || name == "bonus" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", name, v))
	}
	return strings.Join(parts, " ")
}

// sleepCtx 等一段时间；被取消时返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func ri(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// GenPassword 生成满足 Oreate 密码强度（>=8 位、含大小写与数字）的随机密码。
func GenPassword(n int) string {
	const lower = "abcdefghijkmnpqrstuvwxyz"
	const upper = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	const digit = "23456789"
	all := lower + upper + digit
	if n < 12 {
		n = 12
	}
	b := make([]byte, n)
	b[0] = upper[ri(len(upper))]
	b[1] = lower[ri(len(lower))]
	b[2] = digit[ri(len(digit))]
	for i := 3; i < n; i++ {
		b[i] = all[ri(len(all))]
	}
	return string(b)
}
