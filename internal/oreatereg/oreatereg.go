// Package oreatereg 完成 oreateai.com 的全协议注册：注册 → 邮件确认链接 →
// 登录 → 校验自动到账的积分 → 用 Kling3.0 Omini 1k 生成一张图（再自动加赠 50 积分），
// 并采集站点会话 Cookie 供 2api 导出。
// 注册、确认、登录、积分走 HTTP；反爬 token jt 由浏览器现铸（一次性、与 OUID 绑定），
// 生图请求还必须留在浏览器页面里发，否则一律被风控判成 spam user。
package oreatereg

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
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

	// spamRetryDelay 生图被风控拦下后重试前的等待时间。
	spamRetryDelay = 45 * time.Second

	// imageWaitTimeout 等生图 SSE 流出图的最长时间（实测约 25s）。
	imageWaitTimeout = 3 * time.Minute

	// welcomePoints/dailyPoints 是新号注册后自动到账的两笔积分（欢迎 50、每日额度 30）。
	welcomePoints = 50
	dailyPoints   = 30
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

	// 浏览器会话要一直开到生图结束：注册的 jt 可以拿出来走 HTTP，
	// 但生图请求必须留在这个页面里发。
	in.logf("正在启动浏览器会话")
	sess, err := openSession(ctx, in)
	if err != nil {
		return nil, err
	}
	defer sess.close()
	signupJT, err := sess.mint(ctx, in)
	if err != nil {
		return nil, err
	}

	cli, err := newClient(in.Proxy, sess.UA)
	if err != nil {
		return nil, err
	}
	// jt 与铸造它的设备绑定，HTTP 会话必须带同一个 OUID。
	cli.setCookie("OUID", sess.OUID)

	t, err := cli.ticket(ctx)
	if err != nil {
		return nil, err
	}
	encPassword, err := encryptPassword(t.PK, in.Password)
	if err != nil {
		return nil, err
	}
	signup, err := cli.signup(ctx, in.Email, encPassword, t.TicketID, signupJT)
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

	// 生图要在浏览器里发，把会话票据注回页面让它变成已登录状态。
	if err = sess.useSession(cookies["ouss"]); err != nil {
		return nil, fmt.Errorf("向浏览器注入会话票据失败: %w", err)
	}

	// 每日额度（daily 30）要请求一次用户信息才会发到账，登录后先拉一次。
	cli.touchUserInfo(ctx)

	// 注册赠分是异步到账的，等它到账再生图，否则会因积分不足出图失败。
	detail, total, err := waitPoints(ctx, cli)
	if err != nil {
		return nil, err
	}
	in.logf("注册自动到账积分: %d（%s）", total, formatPoints(detail))

	imageURL, genErr := generate(ctx, cli, in, sess)
	if genErr != nil {
		in.logf("生图失败: %v（账号已注册可用，积分加赠未完成）", genErr)
	}

	if genErr == nil {
		// 出图赠分也是异步到账的，等它到账再记录，否则导出的积分会少 50。
		cli.touchUserInfo(ctx)
		detail, total, err = waitPointsAbove(ctx, cli, total)
	} else {
		detail, total, err = cli.points(ctx)
	}
	if err != nil {
		return nil, err
	}
	in.logf("生图后积分: %d（%s）", total, formatPoints(detail))

	cookies = cli.cookies()
	cookies["OUID"] = sess.OUID
	return &Result{
		Email:       in.Email,
		Password:    in.Password,
		Cookies:     cookies,
		OUID:        sess.OUID,
		UserAgent:   sess.UA,
		PointDetail: detail,
		Points:      total,
		ImageURL:    imageURL,
		ImageModel:  imageModel + " " + imageResolution,
		CapturedAt:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// generate 建会话并在浏览器页面里出一张图，出图后站点自动加赠 50 积分。
// 被风控判成 spam user 时重新加载页面、换一个新铸的 jt 再试一次。
func generate(ctx context.Context, cli *client, in Input, sess *session) (string, error) {
	chatID, err := cli.createChat(ctx)
	if err != nil {
		return "", err
	}
	in.logf("已创建生图会话，调用 %s %s（%s）", imageModel, imageResolution, imageRatio)
	imageURL, err := sess.generateImage(ctx, chatID, defaultPrompt, in.Email)
	if errors.Is(err, ErrSpamRejected) {
		in.logf("生图被风控拦下，%s 后换新的反爬 token 重试一次", spamRetryDelay)
		if !sleepCtx(ctx, spamRetryDelay) {
			return imageURL, ctx.Err()
		}
		imageURL, err = sess.generateImage(ctx, chatID, defaultPrompt, in.Email)
	}
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
		// 欢迎赠分 50 与每日额度 30 分两笔到账，等两笔都到再继续。
		if total >= welcomePoints+dailyPoints || time.Now().After(deadline) {
			return detail, total, nil
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return detail, total, ctx.Err()
		}
	}
}

// waitPointsAbove 轮询积分，等出图赠分到账（读到比 base 大的余额即返回）。
// restPoint 有缓存，出图后立刻读会读到旧值。
func waitPointsAbove(ctx context.Context, cli *client, base int) (map[string]int, int, error) {
	deadline := time.Now().Add(pointsWaitTimeout)
	for {
		detail, total, err := cli.points(ctx)
		if err != nil {
			return nil, 0, err
		}
		if total > base || time.Now().After(deadline) {
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
