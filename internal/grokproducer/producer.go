package grokproducer

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/grokreg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"gorm.io/gorm"
)

const (
	codeWaitTimeout  = 10 * time.Minute
	codePollTimeout  = 4 * time.Minute
	codePollInterval = 5 * time.Second
	maxLogBytes      = 64 * 1024
)

var (
	digitCodeRe = regexp.MustCompile(`\b(\d{6})\b`)
	xaiCodeRe   = regexp.MustCompile(`(?i)\b([a-z0-9]{3})-([a-z0-9]{3})\b`)
)

type Producer struct {
	db   *gorm.DB
	mail *mailfetch.Client

	mu      sync.Mutex
	waiters map[uint]chan string
	cancel  map[uint]context.CancelFunc
	pxIdx   int
}

func New(db *gorm.DB, mail *mailfetch.Client) *Producer {
	return &Producer{
		db:      db,
		mail:    mail,
		waiters: map[uint]chan string{},
		cancel:  map[uint]context.CancelFunc{},
	}
}

func (p *Producer) Start(email, note string) (*models.GrokRegistration, error) {
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

	var existing models.GrokRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		if existing.Status == "registering" || existing.Status == "waiting_code" {
			return nil, fmt.Errorf("该邮箱的 Grok 注册正在进行中")
		}
		existing.Status = "registering"
		existing.Note = note
		existing.MailboxID = mailboxID
		existing.Password = grokreg.GenPassword(16)
		existing.AuthData = ""
		existing.Shot = nil
		existing.Shipped = false
		if err := p.db.Save(&existing).Error; err != nil {
			return nil, err
		}
		go p.run(existing.ID)
		return &existing, nil
	}

	reg := models.GrokRegistration{Email: email, MailboxID: mailboxID, Password: grokreg.GenPassword(16), Status: "registering", Note: note}
	if err := p.db.Create(&reg).Error; err != nil {
		return nil, err
	}
	go p.run(reg.ID)
	return &reg, nil
}

func (p *Producer) StartFromAccounts(count int) ([]models.GrokRegistration, error) {
	if count < 1 {
		return nil, fmt.Errorf("数量必须 >= 1")
	}
	var accounts []models.Registration
	if err := p.db.
		Where("status = ? AND mailbox_id <> 0", "registered").
		Where("email NOT IN (?)",
			p.db.Model(&models.GrokRegistration{}).
				Select("email").
				Where("status IN ?", []string{"registered", "registering", "waiting_code"})).
		Order("id asc").
		Limit(count).
		Find(&accounts).Error; err != nil {
		return nil, err
	}

	started := make([]models.GrokRegistration, 0, len(accounts))
	for _, acc := range accounts {
		reg := models.GrokRegistration{
			Email:     acc.Email,
			MailboxID: acc.MailboxID,
			Password:  grokreg.GenPassword(16),
			Status:    "registering",
			Note:      fmt.Sprintf("来源: ChatGPT账号 #%d，自动读取验证码", acc.ID),
		}
		var existing models.GrokRegistration
		if err := p.db.Where("email = ?", acc.Email).First(&existing).Error; err == nil {
			existing.MailboxID = acc.MailboxID
			existing.Status = "registering"
			existing.Shipped = false
			existing.Password = reg.Password
			existing.Note = reg.Note
			existing.AuthData = ""
			existing.Shot = nil
			if err := p.db.Save(&existing).Error; err != nil {
				return started, err
			}
			reg = existing
		} else if err := p.db.Create(&reg).Error; err != nil {
			return started, err
		}
		started = append(started, reg)
		go p.run(reg.ID)
	}
	if len(started) >= count {
		return started, nil
	}

	var mailboxes []models.Mailbox
	if err := p.db.
		Where("status = ?", "verified").
		Where("email NOT IN (?)",
			p.db.Model(&models.GrokRegistration{}).
				Select("email").
				Where("status IN ?", []string{"registered", "registering", "waiting_code"})).
		Order("id asc").
		Limit(count - len(started)).
		Find(&mailboxes).Error; err != nil {
		return started, err
	}
	for _, mb := range mailboxes {
		reg := models.GrokRegistration{
			Email:     mb.Email,
			MailboxID: mb.ID,
			Password:  grokreg.GenPassword(16),
			Status:    "registering",
			Note:      fmt.Sprintf("来源: 邮箱管理 #%d，自动读取验证码", mb.ID),
		}
		var existing models.GrokRegistration
		if err := p.db.Where("email = ?", mb.Email).First(&existing).Error; err == nil {
			existing.MailboxID = mb.ID
			existing.Status = "registering"
			existing.Shipped = false
			existing.Password = reg.Password
			existing.Note = reg.Note
			existing.AuthData = ""
			existing.Shot = nil
			if err := p.db.Save(&existing).Error; err != nil {
				return started, err
			}
			reg = existing
		} else if err := p.db.Create(&reg).Error; err != nil {
			return started, err
		}
		started = append(started, reg)
		go p.run(reg.ID)
	}
	if len(started) == 0 {
		return nil, fmt.Errorf("账号管理和邮箱管理里都没有可用于 Grok 注册的账号")
	}
	return started, nil
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

	var reg models.GrokRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return
	}
	p.appendLog(id, "开始 Grok 邮箱注册")
	since := time.Now().Add(-30 * time.Second)

	in := grokreg.Input{
		Email:    reg.Email,
		Password: reg.Password,
		Proxy:    p.nextProxy(),
		// Match the reference project: Grok registration is headed by default.
		// A dedicated opt-in setting can still enable headless for diagnostics.
		Headless:        p.getSetting("grok_headless") == "1",
		CaptchaProvider: p.getSetting("captcha_provider"),
		CaptchaAPIKey:   p.getSetting("captcha_api_key"),
		Log: func(f string, a ...any) {
			p.appendLog(id, fmt.Sprintf(f, a...))
		},
		WaitCode: func(ctx context.Context) (string, error) {
			if reg.MailboxID != 0 {
				return p.fetchCode(ctx, id, reg.MailboxID, since)
			}
			return p.waitManualCode(ctx, id)
		},
		SaveShot: func(png []byte) {
			p.db.Model(&models.GrokRegistration{}).Where("id = ?", id).Update("shot", png)
		},
	}

	res, err := grokreg.Register(ctx, in)
	if err != nil {
		p.appendLog(id, "注册失败: "+err.Error())
		p.db.Model(&models.GrokRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   truncateStr(err.Error(), 500),
		})
		return
	}

	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.appendLog(id, "Grok 注册成功")
	p.db.Model(&models.GrokRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"status":    "registered",
		"auth_data": string(authBytes),
	})
}

func (p *Producer) waitManualCode(ctx context.Context, id uint) (string, error) {
	ch := make(chan string, 1)
	p.mu.Lock()
	p.waiters[id] = ch
	p.mu.Unlock()
	p.db.Model(&models.GrokRegistration{}).Where("id = ?", id).Update("status", "waiting_code")

	timer := time.NewTimer(codeWaitTimeout)
	defer timer.Stop()
	select {
	case code := <-ch:
		p.db.Model(&models.GrokRegistration{}).Where("id = ?", id).Update("status", "registering")
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
	p.appendLog(id, "开始自动读取 Grok 邮件验证码")
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := p.mail.ListMessages(ctx, acc, 15)
		if err == nil {
			for _, m := range msgs {
				if m.ReceivedAt.Before(since) || !looksLikeGrok(m) {
					continue
				}
				if code := extractGrokCode(m.Subject); code != "" {
					p.appendLog(id, "已从邮件标题读取验证码并自动提交")
					return code, nil
				}
				full, gerr := p.mail.GetMessage(ctx, acc, m.ID)
				if gerr != nil {
					continue
				}
				if code := extractGrokCode(full.Subject + " " + full.Text); code != "" {
					p.appendLog(id, "已从邮件正文读取验证码并自动提交")
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
	return "", fmt.Errorf("超时未收到 Grok 验证码邮件")
}

func extractGrokCode(s string) string {
	if code := xaiCodeRe.FindStringSubmatch(s); code != nil {
		return strings.ToUpper(code[1] + code[2])
	}
	if code := digitCodeRe.FindStringSubmatch(s); code != nil {
		return code[1]
	}
	return ""
}

func looksLikeGrok(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "grok") ||
		strings.Contains(s, "x.ai") ||
		strings.Contains(s, "xai") ||
		strings.Contains(s, "security code") ||
		strings.Contains(s, "verification")
}

func (p *Producer) appendLog(id uint, line string) {
	stamp := time.Now().Format("2006-01-02 15:04:05")
	var reg models.GrokRegistration
	if err := p.db.Select("log").First(&reg, id).Error; err != nil {
		return
	}
	log := reg.Log
	if strings.TrimSpace(log) == "" {
		log = ""
	} else if !strings.HasSuffix(log, "\n") {
		log += "\n"
	}
	log += stamp + " " + line + "\n"
	if len(log) > maxLogBytes {
		log = log[len(log)-maxLogBytes:]
	}
	p.db.Model(&models.GrokRegistration{}).Where("id = ?", id).Update("log", log)
}

func (p *Producer) getSetting(key string) string {
	var s models.Setting
	if err := p.db.Where("key = ?", key).First(&s).Error; err != nil {
		return ""
	}
	return s.Value
}

func (p *Producer) nextProxy() string {
	// Prefer Grok-specific settings when they exist. The current settings page
	// only writes the global proxy keys, so inherit those keys by default.
	// An explicitly configured Grok switch still overrides the global switch.
	enabled := strings.TrimSpace(p.getSetting("grok_proxy_enabled"))
	raw := p.getSetting("grok_proxy_list")
	if enabled == "" {
		enabled = strings.TrimSpace(p.getSetting("proxy_enabled"))
		raw = p.getSetting("proxy_list")
	} else if strings.TrimSpace(raw) == "" {
		// A dedicated Grok switch with no dedicated list means: keep Grok's
		// enablement independent, but always follow the latest global list.
		raw = p.getSetting("proxy_list")
	}
	if enabled != "1" {
		return ""
	}
	proxies := proxyList(raw)
	if len(proxies) == 0 {
		return ""
	}
	p.mu.Lock()
	proxy := proxies[p.pxIdx%len(proxies)]
	p.pxIdx++
	p.mu.Unlock()
	return proxy
}

func proxyList(raw string) []string {
	raw = strings.ReplaceAll(raw, ",", "\n")
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
