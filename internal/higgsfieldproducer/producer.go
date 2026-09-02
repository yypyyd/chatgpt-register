// Package higgsfieldproducer 批量生产 higgsfield.ai 账号：取号、注册、收码、
// 落库，并在注册完成后按需继续跑 pricing 页的绑卡优惠流程。
package higgsfieldproducer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/captcha"
	"chatgpt-register/internal/higgsfieldreg"
	"chatgpt-register/internal/leonardoreg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/prodcore"

	"gorm.io/gorm"
)

const (
	codeWaitTimeout  = 10 * time.Minute
	codePollTimeout  = 4 * time.Minute
	codePollInterval = 2 * time.Second
	maxLogBytes      = 64 * 1024

	// defaultRetryCooldownMin 注册失败后重试同一邮箱前的等待分钟数。
	defaultRetryCooldownMin = 30
)

var digitCodeRe = regexp.MustCompile(`\b(\d{6})\b`)

type Producer struct {
	*prodcore.Core

	db   *gorm.DB
	mail *mailfetch.Client

	mu      sync.Mutex
	waiters map[uint]chan string
	cancel  map[uint]context.CancelFunc
	// topUpCancel 非空表示补任务循环在跑，StopAll 用它停掉补任务。
	topUpCancel context.CancelFunc
	runTarget   int
	runTracked  []uint
}

func New(db *gorm.DB, mail *mailfetch.Client) *Producer {
	return &Producer{
		Core:    prodcore.New(db),
		db:      db,
		mail:    mail,
		waiters: map[uint]chan string{},
		cancel:  map[uint]context.CancelFunc{},
	}
}

func (p *Producer) Start(email, note string) (*models.HiggsfieldRegistration, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("邮箱不能为空")
	}
	var mb models.Mailbox
	mailboxID := uint(0)
	if err := p.db.Where("email = ? AND status = ?", email, "verified").First(&mb).Error; err == nil {
		mailboxID = mb.ID
		if strings.TrimSpace(note) == "" {
			note = fmt.Sprintf("来源: 邮箱管理 #%d，自动读取验证码", mb.ID)
		}
	}

	var existing models.HiggsfieldRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		if existing.Status == "registering" || existing.Status == "waiting_code" {
			return nil, fmt.Errorf("该邮箱的 Higgsfield 注册正在进行中")
		}
		existing.Status = "registering"
		existing.Note = note
		existing.MailboxID = mailboxID
		existing.Password = higgsfieldreg.GenPassword(16)
		existing.AuthData = ""
		existing.TrialStatus = ""
		existing.CheckoutURL = ""
		existing.Shipped = false
		if err := p.db.Save(&existing).Error; err != nil {
			return nil, err
		}
		go p.run(existing.ID)
		return &existing, nil
	}

	reg := models.HiggsfieldRegistration{
		Email:     email,
		MailboxID: mailboxID,
		Password:  higgsfieldreg.GenPassword(16),
		Status:    "registering",
		Note:      note,
	}
	if err := p.db.Create(&reg).Error; err != nil {
		return nil, err
	}
	go p.run(reg.ID)
	return &reg, nil
}

// StartFromAccounts 从账号管理 / 邮箱管理取号开工，并在后台补任务，直到本次拿到
// count 个已注册账号或再也没有可用邮箱。
func (p *Producer) StartFromAccounts(count int) ([]models.HiggsfieldRegistration, error) {
	if count < 1 {
		return nil, fmt.Errorf("数量必须 >= 1")
	}
	started, cooling, err := p.claimTargets(min(count, p.MaxConcurrency()))
	if err != nil {
		return nil, err
	}
	if len(started) == 0 {
		if cooling {
			return nil, fmt.Errorf("可重试的邮箱都还在失败冷却中，请稍后再试")
		}
		return nil, fmt.Errorf("账号管理和邮箱管理里都没有可用于 Higgsfield 注册的账号")
	}
	p.beginRun(count, started)
	for _, reg := range started {
		go p.run(reg.ID)
	}
	go p.topUp(count, started)
	return started, nil
}

// claimTargets 取最多 count 个可注册的邮箱并置为 registering。
func (p *Producer) claimTargets(count int) ([]models.HiggsfieldRegistration, bool, error) {
	cutoff := time.Now().Add(-p.retryCooldown())
	blocked := p.db.Model(&models.HiggsfieldRegistration{}).
		Select("email").
		Where("status <> ? OR updated_at > ?", "register_failed", cutoff)

	started := make([]models.HiggsfieldRegistration, 0, count)
	// 优先用已注册 ChatGPT 母号（邮箱已在池里且可读验证码）。
	var accounts []models.Registration
	if err := p.db.
		Where("status = ? AND mailbox_id <> 0 AND is_mother = ?", "registered", true).
		Where("email NOT IN (?)", blocked).
		Order("id asc").
		Limit(count).
		Find(&accounts).Error; err != nil {
		return nil, false, err
	}
	for _, acc := range accounts {
		reg, err := p.claimOne(acc.Email, acc.MailboxID,
			fmt.Sprintf("来源: ChatGPT账号 #%d，自动读取验证码", acc.ID))
		if err != nil {
			return started, false, err
		}
		started = append(started, *reg)
	}
	if len(started) < count {
		var mailboxes []models.Mailbox
		if err := p.db.
			Where("status = ?", "verified").
			Where("email NOT IN (?)", blocked).
			Order("id asc").
			Limit(count - len(started)).
			Find(&mailboxes).Error; err != nil {
			return started, false, err
		}
		for _, mb := range mailboxes {
			reg, err := p.claimOne(mb.Email, mb.ID,
				fmt.Sprintf("来源: 邮箱管理 #%d，自动读取验证码", mb.ID))
			if err != nil {
				return started, false, err
			}
			started = append(started, *reg)
		}
	}
	if len(started) > 0 {
		return started, false, nil
	}
	return started, p.hasCoolingFailure(cutoff), nil
}

func (p *Producer) claimOne(email string, mailboxID uint, note string) (*models.HiggsfieldRegistration, error) {
	reg := models.HiggsfieldRegistration{
		Email:     email,
		MailboxID: mailboxID,
		Password:  higgsfieldreg.GenPassword(16),
		Status:    "registering",
		Note:      note,
	}
	var existing models.HiggsfieldRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		existing.MailboxID = mailboxID
		existing.Status = "registering"
		existing.Shipped = false
		existing.Password = reg.Password
		existing.Note = note
		existing.AuthData = ""
		existing.TrialStatus = ""
		existing.CheckoutURL = ""
		if err := p.db.Save(&existing).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}
	if err := p.db.Create(&reg).Error; err != nil {
		return nil, err
	}
	return &reg, nil
}

func (p *Producer) hasCoolingFailure(cutoff time.Time) bool {
	var n int64
	p.db.Model(&models.HiggsfieldRegistration{}).
		Where("status = ? AND updated_at > ?", "register_failed", cutoff).
		Count(&n)
	return n > 0
}

func (p *Producer) retryCooldown() time.Duration {
	return p.SettingMinutes("retry_cooldown_min", defaultRetryCooldownMin)
}

// topUp 边跑边补：维持在跑任务数在并发上限，直到拿到 count 个已注册账号。
func (p *Producer) topUp(count int, started []models.HiggsfieldRegistration) {
	ctx, ok := p.beginTopUp()
	if !ok {
		return
	}
	defer p.endTopUp()

	tracked := p.trackedIDs()
	for {
		if !prodcore.Sleep(ctx, 5*time.Second) {
			return
		}
		running := p.runningNum()
		remaining := count - p.countRegistered(tracked) - running
		if remaining <= 0 {
			if running > 0 {
				continue
			}
			return
		}
		slots := p.MaxConcurrency() - running
		if slots <= 0 {
			continue
		}
		regs, cooling, err := p.claimTargets(min(remaining, slots))
		if err != nil {
			return
		}
		if len(regs) == 0 {
			if running > 0 {
				continue
			}
			if cooling && prodcore.Sleep(ctx, 60*time.Second) {
				continue
			}
			return
		}
		p.trackRun(regs)
		tracked = p.trackedIDs()
		for _, reg := range regs {
			go p.run(reg.ID)
		}
	}
}

func (p *Producer) beginRun(count int, started []models.HiggsfieldRegistration) {
	p.mu.Lock()
	p.runTarget = count
	p.runTracked = make([]uint, 0, count)
	for _, reg := range started {
		p.runTracked = append(p.runTracked, reg.ID)
	}
	p.mu.Unlock()
}

func (p *Producer) trackRun(regs []models.HiggsfieldRegistration) {
	p.mu.Lock()
	for _, reg := range regs {
		p.runTracked = append(p.runTracked, reg.ID)
	}
	p.mu.Unlock()
}

func (p *Producer) trackedIDs() []uint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint(nil), p.runTracked...)
}

func (p *Producer) beginTopUp() (context.Context, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.topUpCancel != nil {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.topUpCancel = cancel
	return ctx, true
}

func (p *Producer) endTopUp() {
	p.mu.Lock()
	p.runTarget = 0
	if p.topUpCancel != nil {
		p.topUpCancel()
		p.topUpCancel = nil
	}
	p.mu.Unlock()
}

func (p *Producer) runningNum() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.cancel)
}

func (p *Producer) countRegistered(ids []uint) int {
	if len(ids) == 0 {
		return 0
	}
	var n int64
	p.db.Model(&models.HiggsfieldRegistration{}).
		Where("id IN ? AND status = ?", ids, "registered").
		Count(&n)
	return int(n)
}

func (p *Producer) SubmitCode(id uint, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("验证码不能为空")
	}
	p.mu.Lock()
	ch := p.waiters[id]
	p.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("该任务当前不在等待验证码")
	}
	select {
	case ch <- code:
		p.appendLog(id, "已收到页面提交的验证码")
		return nil
	default:
		return fmt.Errorf("验证码已提交，请等待任务继续")
	}
}

func (p *Producer) Stop(id uint) {
	p.mu.Lock()
	cancel := p.cancel[id]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// StopAll 停止所有在跑的 Higgsfield 注册任务与补任务循环。
func (p *Producer) StopAll() {
	p.endTopUp()
	p.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(p.cancel))
	for _, cancel := range p.cancel {
		cancels = append(cancels, cancel)
	}
	p.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// Progress 生产进度快照，供 /api/higgsfield/produce/status 展示。
type Progress struct {
	Running    bool `json:"running"`
	Pending    int  `json:"pending"`
	RunningNum int  `json:"running_num"`
	Registered int  `json:"registered"`
	Failed     int  `json:"failed"`
}

func (p *Producer) Snapshot() Progress {
	p.mu.Lock()
	runningNum := len(p.cancel)
	topUp := p.topUpCancel != nil
	p.mu.Unlock()

	count := func(statuses ...string) int {
		var n int64
		p.db.Model(&models.HiggsfieldRegistration{}).Where("status IN ?", statuses).Count(&n)
		return int(n)
	}
	return Progress{
		Running:    runningNum > 0 || topUp,
		Pending:    p.pendingRemaining(runningNum),
		RunningNum: runningNum,
		Registered: count("registered"),
		Failed:     count("register_failed"),
	}
}

func (p *Producer) pendingRemaining(runningNum int) int {
	p.mu.Lock()
	target := p.runTarget
	tracked := append([]uint(nil), p.runTracked...)
	p.mu.Unlock()
	if target <= 0 {
		return 0
	}
	remaining := target - p.countRegistered(tracked) - runningNum
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (p *Producer) run(id uint) {
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel[id] = cancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, id)
		delete(p.cancel, id)
		p.mu.Unlock()
	}()

	var reg models.HiggsfieldRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return
	}

	if !p.acquireSlot(ctx, id) {
		p.appendLog(id, "已取消（排队等待空闲注册槽位时被停止）")
		p.db.Model(&models.HiggsfieldRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}
	defer p.ReleaseSlot()

	p.appendLog(id, "开始 Higgsfield 邮箱注册（Clerk 协议）")
	since := time.Now().Add(-30 * time.Second)
	proxy := p.NextProxy()

	in := higgsfieldreg.Input{
		Email:    reg.Email,
		Password: reg.Password,
		Proxy:    proxy,
		Log: func(f string, a ...any) {
			p.appendLog(id, fmt.Sprintf(f, a...))
		},
		MintToken: func(ctx context.Context, sitekey string) (string, error) {
			return p.mintToken(ctx, id, sitekey, proxy)
		},
		WaitCode: func(ctx context.Context) (string, error) {
			if reg.MailboxID != 0 {
				return p.fetchCode(ctx, id, reg.MailboxID, since)
			}
			return p.waitManualCode(ctx, id)
		},
	}

	res, err := higgsfieldreg.Register(ctx, in)
	if err != nil {
		p.appendLog(id, "注册失败: "+err.Error())
		status := "register_failed"
		if errors.Is(err, higgsfieldreg.ErrEmailTaken) {
			status = "already_registered"
		}
		p.db.Model(&models.HiggsfieldRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": status,
			"note":   prodcore.Truncate(err.Error(), 500),
		})
		return
	}

	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.appendLog(id, "Higgsfield 注册成功")
	p.db.Model(&models.HiggsfieldRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"status":    "registered",
		"auth_data": string(authBytes),
	})

	// 绑卡优惠默认不自动跑（要真实银行卡）；设置页 higgsfield_trial_auto=1 时
	// 注册完顺手把收银台开出来，停在填卡那一步。
	if p.SettingOn("higgsfield_trial_auto") {
		if _, terr := p.runTrial(ctx, id, res.SessionToken, proxy); terr != nil {
			p.appendLog(id, "绑卡优惠流程未完成: "+terr.Error())
		}
	}
}

// mintToken 产出一枚 Turnstile token：配了打码平台 key 就走打码（机房出口也能过），
// 否则用本机浏览器真实点选。
func (p *Producer) mintToken(ctx context.Context, id uint, sitekey, proxy string) (string, error) {
	if key := strings.TrimSpace(p.Setting("captcha_2captcha_key")); key != "" &&
		p.Setting("higgsfield_captcha") != "browser" {
		p.appendLog(id, "用 2Captcha 解 Turnstile")
		solver := &captcha.TwoCaptcha{Key: key, Log: func(f string, a ...any) {
			p.appendLog(id, fmt.Sprintf(f, a...))
		}}
		return solver.SolveTurnstile(ctx, sitekey, higgsfieldreg.SignUpPageURL, "", "")
	}
	p.appendLog(id, "用本机浏览器真实点选 Turnstile")
	return leonardoreg.MintTurnstile(ctx, leonardoreg.MintOptions{
		PageURL:  higgsfieldreg.SignUpPageURL,
		Sitekey:  sitekey,
		Proxy:    proxy,
		Headless: p.SettingOn("higgsfield_headless"),
		Log: func(f string, a ...any) {
			p.appendLog(id, fmt.Sprintf(f, a...))
		},
	})
}

// RunTrial 对某个已注册账号跑绑卡优惠流程（供页面按钮调用）：会话过期时用导出的
// __client cookie 刷新，跑到需要真实卡信息为止。
func (p *Producer) RunTrial(id uint) (*higgsfieldreg.TrialResult, error) {
	var reg models.HiggsfieldRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return nil, fmt.Errorf("记录不存在")
	}
	if reg.Status != "registered" || strings.TrimSpace(reg.AuthData) == "" {
		return nil, fmt.Errorf("该账号还没有可用会话，先完成注册")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	token, err := p.freshToken(ctx, reg)
	if err != nil {
		return nil, err
	}
	return p.runTrial(ctx, id, token, p.NextProxy())
}

// freshToken 取一枚可用的会话 JWT：Clerk 的 JWT 只有 60 秒，先用 __client 刷新。
func (p *Producer) freshToken(ctx context.Context, reg models.HiggsfieldRegistration) (string, error) {
	var auth struct {
		SessionToken string `json:"session_token"`
		Cookies      []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal([]byte(reg.AuthData), &auth); err != nil {
		return "", fmt.Errorf("解析账号会话失败: %w", err)
	}
	for _, ck := range auth.Cookies {
		if ck.Name != "__client" || ck.Value == "" {
			continue
		}
		token, err := higgsfieldreg.RefreshSession(ctx, p.NextProxy(), ck.Value)
		if err == nil {
			return token, nil
		}
		p.appendLog(reg.ID, "刷新会话失败，改用注册时的 token: "+err.Error())
		break
	}
	if auth.SessionToken == "" {
		return "", fmt.Errorf("账号里没有可用会话 token")
	}
	return auth.SessionToken, nil
}

// runTrial 跑绑卡优惠：查资格 → 开 Stripe 收银台，把状态与收银台地址落库。
// 不提交任何卡信息。
func (p *Producer) runTrial(ctx context.Context, id uint, token, proxy string) (*higgsfieldreg.TrialResult, error) {
	p.appendLog(id, "开始 pricing 绑卡优惠流程")
	res, err := higgsfieldreg.StartTrial(ctx, higgsfieldreg.TrialInput{
		SessionToken: token,
		Proxy:        proxy,
		Log: func(f string, a ...any) {
			p.appendLog(id, fmt.Sprintf(f, a...))
		},
	})
	if err != nil {
		p.db.Model(&models.HiggsfieldRegistration{}).Where("id = ?", id).
			Update("trial_status", "failed")
		return nil, err
	}
	p.db.Model(&models.HiggsfieldRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"trial_status": res.State,
		"checkout_url": res.CheckoutURL,
	})
	switch res.State {
	case "need_card":
		p.appendLog(id, "收银台已开好，等待填入真实银行卡信息（本程序不会自行提交卡号）")
	case "already_active":
		p.appendLog(id, "该账号已经在试用中，无需再领")
	case "not_eligible":
		p.appendLog(id, "该账号不符合绑卡优惠条件")
	}
	return res, nil
}

func (p *Producer) waitManualCode(ctx context.Context, id uint) (string, error) {
	ch := make(chan string, 1)
	p.mu.Lock()
	p.waiters[id] = ch
	p.mu.Unlock()
	p.db.Model(&models.HiggsfieldRegistration{}).Where("id = ?", id).Update("status", "waiting_code")

	timer := time.NewTimer(codeWaitTimeout)
	defer timer.Stop()
	select {
	case code := <-ch:
		p.db.Model(&models.HiggsfieldRegistration{}).Where("id = ?", id).Update("status", "registering")
		return code, nil
	case <-timer.C:
		return "", fmt.Errorf("等待验证码超时")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *Producer) fetchCode(ctx context.Context, id, mailboxID uint, since time.Time) (string, error) {
	var mb models.Mailbox
	if err := p.db.First(&mb, mailboxID).Error; err != nil {
		return "", fmt.Errorf("读取邮箱凭据失败: %w", err)
	}
	acc := mailfetch.Account{Email: mb.Email, ClientID: mb.ClientID, RefreshToken: mb.RefreshToken}
	deadline := time.Now().Add(codePollTimeout)
	p.appendLog(id, "开始自动读取 Higgsfield 邮件验证码")
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := p.mail.ListMessages(ctx, acc, 15)
		if err == nil {
			for _, m := range msgs {
				if m.ReceivedAt.Before(since) || !looksLikeHiggsfield(m) {
					continue
				}
				if code := extractCode(m.Subject); code != "" {
					p.appendLog(id, "已从邮件标题读取验证码")
					return code, nil
				}
				full, gerr := p.mail.GetMessage(ctx, acc, m.ID)
				if gerr != nil {
					continue
				}
				if code := extractCode(full.Subject + " " + full.Text); code != "" {
					p.appendLog(id, "已从邮件正文读取验证码")
					return code, nil
				}
			}
		} else {
			p.appendLog(id, "读取邮件暂时失败，继续重试: "+err.Error())
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(codePollInterval):
		}
	}
	return "", fmt.Errorf("超时未收到 Higgsfield 验证码邮件")
}

func extractCode(s string) string {
	if code := digitCodeRe.FindStringSubmatch(s); code != nil {
		return code[1]
	}
	return ""
}

func looksLikeHiggsfield(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "higgsfield") ||
		strings.Contains(s, "clerk") ||
		strings.Contains(s, "verification code") ||
		strings.Contains(s, "verify your") ||
		strings.Contains(s, "one-time")
}

func (p *Producer) appendLog(id uint, line string) {
	var reg models.HiggsfieldRegistration
	if err := p.db.Select("log").First(&reg, id).Error; err != nil {
		return
	}
	p.db.Model(&models.HiggsfieldRegistration{}).Where("id = ?", id).
		Update("log", prodcore.AppendLogLine(reg.Log, line, maxLogBytes))
}

// acquireSlot 阻塞直到拿到并发槽位；ctx 取消时返回 false。
func (p *Producer) acquireSlot(ctx context.Context, id uint) bool {
	return p.AcquireSlot(ctx, func() {
		p.appendLog(id, "并发已满，排队等待空闲注册槽位")
	})
}
