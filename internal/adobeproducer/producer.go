package adobeproducer

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/adobereg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"gorm.io/gorm"
)

const (
	codeWaitTimeout  = 10 * time.Minute
	codePollTimeout  = 4 * time.Minute
	codePollInterval = 5 * time.Second
	maxLogBytes      = 64 * 1024

	// defaultMaxConcurrency 未配置任何并发键时的默认值：逐个开工。批量注册时多个
	// 有头浏览器同时抢 CPU 会互相超时，串行最稳。可用设置页「最大并发数」
	// (max_concurrency) 或专用键 adobe_max_concurrency 调大。
	defaultMaxConcurrency = 1
)

var digitCodeRe = regexp.MustCompile(`\b(\d{6})\b`)

type Producer struct {
	db   *gorm.DB
	mail *mailfetch.Client

	mu      sync.Mutex
	waiters map[uint]chan string
	cancel  map[uint]context.CancelFunc
	active  int
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

func (p *Producer) Start(email, note string) (*models.AdobeRegistration, error) {
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

	var existing models.AdobeRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		if existing.Status == "registering" || existing.Status == "waiting_code" {
			return nil, fmt.Errorf("该邮箱的 Adobe 注册正在进行中")
		}
		existing.Status = "registering"
		existing.Note = note
		existing.MailboxID = mailboxID
		existing.Product = "firefly"
		existing.Password = adobereg.GenPassword(16)
		existing.AuthData = ""
		existing.Shot = nil
		existing.Shipped = false
		if err := p.db.Save(&existing).Error; err != nil {
			return nil, err
		}
		go p.run(existing.ID)
		return &existing, nil
	}

	reg := models.AdobeRegistration{Email: email, MailboxID: mailboxID, Product: "firefly", Password: adobereg.GenPassword(16), Status: "registering", Note: note}
	if err := p.db.Create(&reg).Error; err != nil {
		return nil, err
	}
	go p.run(reg.ID)
	return &reg, nil
}

func (p *Producer) StartFromAccounts(count int) ([]models.AdobeRegistration, error) {
	if count < 1 {
		return nil, fmt.Errorf("数量必须 >= 1")
	}
	started := make([]models.AdobeRegistration, 0, count)

	upsert := func(email string, mailboxID uint, note string) error {
		reg := models.AdobeRegistration{
			Email:     email,
			MailboxID: mailboxID,
			Product:   "firefly",
			Password:  adobereg.GenPassword(16),
			Status:    "registering",
			Note:      note,
		}
		var existing models.AdobeRegistration
		if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
			existing.MailboxID = mailboxID
			existing.Product = "firefly"
			existing.Status = "registering"
			existing.Shipped = false
			existing.Password = reg.Password
			existing.Note = note
			existing.AuthData = ""
			existing.Shot = nil
			if err := p.db.Save(&existing).Error; err != nil {
				return err
			}
			reg = existing
		} else if err := p.db.Create(&reg).Error; err != nil {
			return err
		}
		started = append(started, reg)
		go p.run(reg.ID)
		return nil
	}

	// 优先用已注册 ChatGPT 账号（其邮箱已在邮箱池且可读验证码）。
	var accounts []models.Registration
	if err := p.db.
		Where("status = ? AND mailbox_id <> 0", "registered").
		Where("email NOT IN (?)", p.db.Model(&models.AdobeRegistration{}).Select("email")).
		Order("id asc").
		Limit(count).
		Find(&accounts).Error; err != nil {
		return nil, err
	}
	for _, acc := range accounts {
		if err := upsert(acc.Email, acc.MailboxID, fmt.Sprintf("来源: ChatGPT账号 #%d，自动读取验证码", acc.ID)); err != nil {
			return started, err
		}
	}

	// 不足则从已验证邮箱池补齐。
	if len(started) < count {
		var mailboxes []models.Mailbox
		if err := p.db.
			Where("status = ?", "verified").
			Where("email NOT IN (?)", p.db.Model(&models.AdobeRegistration{}).Select("email")).
			Order("id asc").
			Limit(count - len(started)).
			Find(&mailboxes).Error; err != nil {
			return started, err
		}
		for _, mb := range mailboxes {
			if err := upsert(mb.Email, mb.ID, fmt.Sprintf("来源: 邮箱管理 #%d，自动读取验证码", mb.ID)); err != nil {
				return started, err
			}
		}
	}

	if len(started) == 0 {
		return nil, fmt.Errorf("账号管理和邮箱管理里都没有可用于 Adobe 注册的账号")
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

func (p *Producer) StopAll() {
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

// Progress 生产进度快照，供 /api/adobe/produce/status 展示。
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
	p.mu.Unlock()

	count := func(statuses ...string) int {
		var n int64
		p.db.Model(&models.AdobeRegistration{}).Where("status IN ?", statuses).Count(&n)
		return int(n)
	}
	return Progress{
		Running:    runningNum > 0,
		Pending:    count("pending"),
		RunningNum: runningNum,
		Registered: count("registered"),
		Failed:     count("register_failed"),
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

	var reg models.AdobeRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return
	}

	if !p.acquireSlot(ctx, id) {
		p.appendLog(id, "已取消（排队等待空闲注册槽位时被停止）")
		p.db.Model(&models.AdobeRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}
	defer p.releaseSlot()

	p.appendLog(id, "开始 Adobe 邮箱注册")
	since := time.Now().Add(-30 * time.Second)

	in := adobereg.Input{
		Email:    reg.Email,
		Password: reg.Password,
		Proxy:    p.nextProxy(),
		Headless: p.getSetting("adobe_headless") == "1",
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
			p.db.Model(&models.AdobeRegistration{}).Where("id = ?", id).Update("shot", png)
		},
	}

	res, err := adobereg.Register(ctx, in)
	if err != nil {
		p.appendLog(id, "注册失败: "+err.Error())
		p.db.Model(&models.AdobeRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   truncateStr(err.Error(), 500),
		})
		return
	}

	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.appendLog(id, "Adobe 注册成功")
	p.db.Model(&models.AdobeRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"status":    "registered",
		"auth_data": string(authBytes),
	})
}

func (p *Producer) waitManualCode(ctx context.Context, id uint) (string, error) {
	ch := make(chan string, 1)
	p.mu.Lock()
	p.waiters[id] = ch
	p.mu.Unlock()
	p.db.Model(&models.AdobeRegistration{}).Where("id = ?", id).Update("status", "waiting_code")

	timer := time.NewTimer(codeWaitTimeout)
	defer timer.Stop()
	select {
	case code := <-ch:
		p.db.Model(&models.AdobeRegistration{}).Where("id = ?", id).Update("status", "registering")
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
	p.appendLog(id, "开始自动读取 Adobe 邮件验证码")
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := p.mail.ListMessages(ctx, acc, 15)
		if err == nil {
			for _, m := range msgs {
				if m.ReceivedAt.Before(since) || !looksLikeAdobe(m) {
					continue
				}
				if code := extractAdobeCode(m.Subject); code != "" {
					p.appendLog(id, "已从邮件标题读取验证码并自动提交")
					return code, nil
				}
				full, gerr := p.mail.GetMessage(ctx, acc, m.ID)
				if gerr != nil {
					continue
				}
				if code := extractAdobeCode(full.Subject + " " + full.Text); code != "" {
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
	return "", fmt.Errorf("超时未收到 Adobe 验证码邮件")
}

func extractAdobeCode(s string) string {
	if code := digitCodeRe.FindStringSubmatch(s); code != nil {
		return code[1]
	}
	return ""
}

func looksLikeAdobe(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "adobe") ||
		strings.Contains(s, "firefly") ||
		strings.Contains(s, "verification code") ||
		strings.Contains(s, "verify your") ||
		strings.Contains(s, "verify your identity") ||
		strings.Contains(s, "one-time")
}

func (p *Producer) appendLog(id uint, line string) {
	stamp := time.Now().Format("2006-01-02 15:04:05")
	var reg models.AdobeRegistration
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
	p.db.Model(&models.AdobeRegistration{}).Where("id = ?", id).Update("log", log)
}

// maxConcurrency 优先用 Adobe 专用键 adobe_max_concurrency，未设置继承 max_concurrency，默认 1。
func (p *Producer) maxConcurrency() int {
	raw := strings.TrimSpace(p.getSetting("adobe_max_concurrency"))
	if raw == "" {
		raw = strings.TrimSpace(p.getSetting("max_concurrency"))
	}
	n := defaultMaxConcurrency
	if raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			n = parsed
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (p *Producer) acquireSlot(ctx context.Context, id uint) bool {
	logged := false
	for {
		if ctx.Err() != nil {
			return false
		}
		limit := p.maxConcurrency()
		p.mu.Lock()
		if p.active < limit {
			p.active++
			p.mu.Unlock()
			return true
		}
		p.mu.Unlock()
		if !logged {
			p.appendLog(id, "并发已满，排队等待空闲注册槽位")
			logged = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(1 * time.Second):
		}
	}
}

func (p *Producer) releaseSlot() {
	p.mu.Lock()
	if p.active > 0 {
		p.active--
	}
	p.mu.Unlock()
}

func (p *Producer) getSetting(key string) string {
	var s models.Setting
	if err := p.db.Where("key = ?", key).First(&s).Error; err != nil {
		return ""
	}
	return s.Value
}

func (p *Producer) nextProxy() string {
	enabled := strings.TrimSpace(p.getSetting("adobe_proxy_enabled"))
	raw := p.getSetting("adobe_proxy_list")
	if enabled == "" {
		enabled = strings.TrimSpace(p.getSetting("proxy_enabled"))
		raw = p.getSetting("proxy_list")
	} else if strings.TrimSpace(raw) == "" {
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
