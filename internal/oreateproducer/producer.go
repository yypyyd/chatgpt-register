// Package oreateproducer 调度 Oreate AI 的批量注册任务：取号、跑全协议注册、
// 自动从邮箱取确认链接、失败冷却后重试。
package oreateproducer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/oreatereg"
	"chatgpt-register/internal/proxyutil"

	"gorm.io/gorm"
)

const (
	// linkPollTimeout 等确认邮件的总时长，linkPollInterval 为轮询间隔。
	linkPollTimeout  = 3 * time.Minute
	linkPollInterval = 3 * time.Second
	maxLogBytes      = 64 * 1024

	defaultMaxConcurrency   = 1
	defaultRetryCooldownMin = 30

	// badDomainLimit 同一邮箱域名被站点拒收几次后，本次生产不再取该域名的邮箱，
	// 免得把整池同域名邮箱白白消耗掉。
	badDomainLimit = 2

	// defaultLaunchStaggerSec 相邻两个注册任务开始提交的默认最小间隔（秒）。
	// 同一秒内并发提交多个注册时，站点会限流确认邮件（一批往往只有一封准时到，
	// 其余延迟约 10 分钟直接超时），错峰提交可以避开限流。
	defaultLaunchStaggerSec = 45
)

type Producer struct {
	db   *gorm.DB
	mail *mailfetch.Client

	mu     sync.Mutex
	cancel map[uint]context.CancelFunc
	active int
	pxIdx  int
	// topUpCancel 非空表示补任务循环在跑，StopAll 用它停掉补任务。
	topUpCancel context.CancelFunc
	runTarget   int
	runTracked  []uint
	// badDomains 记录被站点拒收的邮箱域名及次数。
	badDomains map[string]int
	// nextLaunch 下一个注册任务最早可以开始提交的时间，用于错峰提交。
	nextLaunch time.Time
}

func New(db *gorm.DB, mail *mailfetch.Client) *Producer {
	return &Producer{db: db, mail: mail, cancel: map[uint]context.CancelFunc{},
		badDomains: map[string]int{}}
}

func (p *Producer) Start(email, note string) (*models.OreateRegistration, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("邮箱不能为空")
	}
	var mb models.Mailbox
	mailboxID := uint(0)
	if err := p.db.Where("email = ? AND status = ?", email, "verified").First(&mb).Error; err == nil {
		mailboxID = mb.ID
		if strings.TrimSpace(note) == "" {
			note = fmt.Sprintf("来源: 邮箱管理 #%d，自动读取确认链接", mb.ID)
		}
	}
	if mailboxID == 0 {
		return nil, fmt.Errorf("该邮箱不在已验证邮箱池里，无法自动读取确认链接")
	}
	reg, err := p.claimOne(email, mailboxID, note)
	if err != nil {
		return nil, err
	}
	go p.run(reg.ID)
	return reg, nil
}

// StartFromAccounts 从账号管理 / 邮箱管理取号开工，并在后台补任务，直到本次拿到
// count 个已注册账号，或者再也没有可用邮箱。
func (p *Producer) StartFromAccounts(count int) ([]models.OreateRegistration, error) {
	if count < 1 {
		return nil, fmt.Errorf("数量必须 >= 1")
	}
	started, cooling, err := p.claimTargets(minInt(count, p.maxConcurrency()))
	if err != nil {
		return nil, err
	}
	if len(started) == 0 {
		if cooling {
			return nil, fmt.Errorf("可重试的邮箱都还在失败冷却中，请稍后再试")
		}
		return nil, fmt.Errorf("账号管理和邮箱管理里都没有可用于 Oreate 注册的账号")
	}
	p.beginRun(count, started)
	for _, reg := range started {
		go p.run(reg.ID)
	}
	go p.topUp(count)
	return started, nil
}

// claimTargets 取最多 count 个可注册的邮箱并置为 registering。
// cooling=true 表示还有失败邮箱可重试、只是没到冷却时间。
func (p *Producer) claimTargets(count int) ([]models.OreateRegistration, bool, error) {
	cutoff := time.Now().Add(-p.retryCooldown())
	blocked := p.db.Model(&models.OreateRegistration{}).
		Select("email").
		Where("status <> ? OR updated_at > ?", "register_failed", cutoff)

	blockedDomains := p.blockedDomains()
	started := make([]models.OreateRegistration, 0, count)
	// 优先用已注册 ChatGPT 母号：其邮箱在邮箱池里、可读确认邮件。
	var accounts []models.Registration
	accQuery := p.db.
		Where("status = ? AND mailbox_id <> 0 AND is_mother = ?", "registered", true).
		Where("email NOT IN (?)", blocked)
	if err := excludeDomains(accQuery, blockedDomains).
		Order("id asc").
		Limit(count).
		Find(&accounts).Error; err != nil {
		return nil, false, err
	}
	for _, acc := range accounts {
		reg, err := p.claimOne(acc.Email, acc.MailboxID,
			fmt.Sprintf("来源: ChatGPT账号 #%d，自动读取确认链接", acc.ID))
		if err != nil {
			return started, false, err
		}
		started = append(started, *reg)
	}
	if len(started) < count {
		var mailboxes []models.Mailbox
		mbQuery := p.db.
			Where("status = ?", "verified").
			Where("email NOT IN (?)", blocked)
		if err := excludeDomains(mbQuery, blockedDomains).
			Order("id asc").
			Limit(count - len(started)).
			Find(&mailboxes).Error; err != nil {
			return started, false, err
		}
		for _, mb := range mailboxes {
			reg, err := p.claimOne(mb.Email, mb.ID,
				fmt.Sprintf("来源: 邮箱管理 #%d，自动读取确认链接", mb.ID))
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

// claimOne 把一个邮箱置为 registering：已有记录就复用（重试同一条），否则新建。
func (p *Producer) claimOne(email string, mailboxID uint, note string) (*models.OreateRegistration, error) {
	password := oreatereg.GenPassword(16)
	var existing models.OreateRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		if existing.Status == "registering" {
			return nil, fmt.Errorf("该邮箱的 Oreate 注册正在进行中")
		}
		existing.MailboxID = mailboxID
		existing.Status = "registering"
		existing.Shipped = false
		existing.Password = password
		existing.Note = note
		existing.AuthData = ""
		existing.Points = 0
		existing.ImageURL = ""
		if err := p.db.Save(&existing).Error; err != nil {
			return nil, err
		}
		return &existing, nil
	}
	reg := models.OreateRegistration{
		Email:     email,
		MailboxID: mailboxID,
		Password:  password,
		Status:    "registering",
		Note:      note,
	}
	if err := p.db.Create(&reg).Error; err != nil {
		return nil, err
	}
	return &reg, nil
}

func (p *Producer) hasCoolingFailure(cutoff time.Time) bool {
	var n int64
	p.db.Model(&models.OreateRegistration{}).
		Where("status = ? AND updated_at > ?", "register_failed", cutoff).
		Count(&n)
	return n > 0
}

func (p *Producer) retryCooldown() time.Duration {
	raw := strings.TrimSpace(p.getSetting("retry_cooldown_min"))
	min := defaultRetryCooldownMin
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			min = n
		}
	}
	return time.Duration(min) * time.Minute
}

// topUp 边跑边补：始终让在跑任务数维持在并发上限，直到本次生产拿到 count 个已注册账号。
func (p *Producer) topUp(count int) {
	ctx, ok := p.beginTopUp()
	if !ok {
		return
	}
	defer p.endTopUp()

	tracked := p.trackedIDs()
	for {
		if !sleepCtx(ctx, 5*time.Second) {
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
		slots := p.maxConcurrency() - running
		if slots <= 0 {
			continue
		}
		regs, cooling, err := p.claimTargets(minInt(remaining, slots))
		if err != nil {
			return
		}
		if len(regs) == 0 {
			if running > 0 {
				continue
			}
			if cooling && sleepCtx(ctx, 60*time.Second) {
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

func (p *Producer) beginRun(count int, started []models.OreateRegistration) {
	p.mu.Lock()
	p.runTarget = count
	p.runTracked = make([]uint, 0, count)
	for _, reg := range started {
		p.runTracked = append(p.runTracked, reg.ID)
	}
	p.mu.Unlock()
}

func (p *Producer) trackRun(regs []models.OreateRegistration) {
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
	p.db.Model(&models.OreateRegistration{}).
		Where("id IN ? AND status = ?", ids, "registered").
		Count(&n)
	return int(n)
}

func (p *Producer) Stop(id uint) {
	p.mu.Lock()
	cancel := p.cancel[id]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// StopAll 请求停止所有在跑的 Oreate 注册任务，并停掉失败重试的补任务循环。
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

// Progress 生产进度快照，供 /api/oreate/produce/status 展示。
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
		p.db.Model(&models.OreateRegistration{}).Where("status IN ?", statuses).Count(&n)
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
		delete(p.cancel, id)
		p.mu.Unlock()
	}()

	var reg models.OreateRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return
	}
	if !p.acquireSlot(ctx, id) {
		p.appendLog(id, "已取消（排队等待空闲注册槽位时被停止）")
		p.db.Model(&models.OreateRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}
	defer p.releaseSlot()

	if !p.waitLaunchTurn(ctx, id) {
		p.appendLog(id, "已取消（错峰等待提交时被停止）")
		p.db.Model(&models.OreateRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}

	p.appendLog(id, "开始 Oreate 全协议注册")
	since := time.Now().Add(-30 * time.Second)

	in := oreatereg.Input{
		Email:    reg.Email,
		Password: reg.Password,
		Proxy:    p.nextProxy(),
		// 站点反爬会识别无头浏览器，铸造 jt 只能有头跑，这里保持有头。
		Headless: false,
		Log: func(f string, a ...any) {
			p.appendLog(id, fmt.Sprintf(f, a...))
		},
		WaitConfirmLink: func(ctx context.Context) (string, error) {
			return p.fetchConfirmLink(ctx, id, reg.MailboxID, since)
		},
	}

	res, err := oreatereg.Register(ctx, in)
	if err != nil {
		p.appendLog(id, "注册失败: "+err.Error())
		status := "register_failed"
		if errors.Is(err, oreatereg.ErrEmailTaken) {
			// 邮箱已被注册是终态，标成 already_registered 后不再进冷却重试。
			status = "already_registered"
		}
		if errors.Is(err, oreatereg.ErrConfirmMailLimited) {
			// 这个邮箱的确认邮件配额用完了，换邮箱才有意义，别再冷却重试。
			status = "email_rejected"
		}
		if errors.Is(err, oreatereg.ErrConfirmMailTimeout) {
			// 重发几次仍超时的邮箱确认链接已过期，重试同一邮箱只会再烧发信配额。
			status = "mail_timeout"
		}
		if errors.Is(err, oreatereg.ErrSignupRejected) {
			// 站点不收这个域名，重试也没用，同时记下域名不再取同域名邮箱。
			status = "email_rejected"
			p.markBadDomain(reg.Email)
		}
		p.db.Model(&models.OreateRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": status,
			"note":   truncateStr(err.Error(), 500),
		})
		return
	}

	authBytes, _ := json.MarshalIndent(res, "", "  ")
	p.appendLog(id, fmt.Sprintf("Oreate 注册成功，积分 %d", res.Points))
	p.db.Model(&models.OreateRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"status":    "registered",
		"auth_data": string(authBytes),
		"points":    res.Points,
		"image_url": res.ImageURL,
	})
}

// launchStagger 跟设置页「注册错峰间隔」(launch_stagger_sec)，未设置默认 45 秒，
// 设 0 表示不错峰、完全并发提交。
func (p *Producer) launchStagger() time.Duration {
	raw := strings.TrimSpace(p.getSetting("launch_stagger_sec"))
	sec := defaultLaunchStaggerSec
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			sec = n
		}
	}
	return time.Duration(sec) * time.Second
}

// waitLaunchTurn 错峰提交：把相邻两次注册提交至少隔开错峰间隔，
// 避免同一秒内并发提交触发站点对确认邮件的限流。
func (p *Producer) waitLaunchTurn(ctx context.Context, id uint) bool {
	stagger := p.launchStagger()
	if stagger <= 0 {
		return true
	}
	p.mu.Lock()
	now := time.Now()
	next := p.nextLaunch
	if next.Before(now) {
		next = now
	}
	p.nextLaunch = next.Add(stagger)
	p.mu.Unlock()
	delay := next.Sub(now)
	if delay <= 0 {
		return true
	}
	p.appendLog(id, fmt.Sprintf("错峰提交：等 %s 再开始，避免确认邮件被站点限流", delay.Round(time.Second)))
	return sleepCtx(ctx, delay)
}

// fetchConfirmLink 轮询邮箱，取出 Oreate 注册确认邮件里的链接。
func (p *Producer) fetchConfirmLink(ctx context.Context, id, mailboxID uint, since time.Time) (string, error) {
	var mb models.Mailbox
	if err := p.db.First(&mb, mailboxID).Error; err != nil {
		return "", fmt.Errorf("读取邮箱凭据失败: %w", err)
	}
	acc := mailfetch.Account{Email: mb.Email, ClientID: mb.ClientID, RefreshToken: mb.RefreshToken}
	deadline := time.Now().Add(linkPollTimeout)
	p.appendLog(id, "开始自动读取 Oreate 确认邮件")
	var recent []mailfetch.Message
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := p.mail.ListMessages(ctx, acc, 15)
		if err == nil {
			recent = recent[:0]
			for _, m := range msgs {
				if m.ReceivedAt.Before(since) {
					continue
				}
				recent = append(recent, m)
				if !looksLikeOreate(m) {
					continue
				}
				full, gerr := p.mail.GetMessage(ctx, acc, m.ID)
				if gerr != nil {
					continue
				}
				if link := oreatereg.ExtractConfirmLink(full.HTML + " " + full.Text); link != "" {
					p.appendLog(id, "已从确认邮件读取注册链接")
					return link, nil
				}
			}
		} else {
			p.appendLog(id, "读取邮件暂时失败，继续重试: "+err.Error())
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(linkPollInterval):
		}
	}
	// 超时时把注册后收到的新邮件列出来，区分是邮件根本没送达还是发件人/主题没匹配上。
	if len(recent) == 0 {
		return "", fmt.Errorf("%w（注册后收件箱/垃圾箱没有任何新邮件）", oreatereg.ErrConfirmMailTimeout)
	}
	summary := make([]string, 0, len(recent))
	for _, m := range recent {
		summary = append(summary, m.From+" | "+m.Subject)
	}
	return "", fmt.Errorf("%w（注册后新邮件: %s）", oreatereg.ErrConfirmMailTimeout, strings.Join(summary, "; "))
}

func looksLikeOreate(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "oreate")
}

func (p *Producer) appendLog(id uint, line string) {
	stamp := time.Now().Format("2006-01-02 15:04:05")
	var reg models.OreateRegistration
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
	p.db.Model(&models.OreateRegistration{}).Where("id = ?", id).Update("log", log)
}

// maxConcurrency 跟设置页「最大并发数」(max_concurrency)，未设置则默认 1。
func (p *Producer) maxConcurrency() int {
	raw := strings.TrimSpace(p.getSetting("max_concurrency"))
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

// nextProxy 跟设置页上的全局代理开关与代理列表，按任务轮换出口。
func (p *Producer) nextProxy() string {
	if strings.TrimSpace(p.getSetting("proxy_enabled")) != "1" {
		return ""
	}
	proxies := proxyList(p.getSetting("proxy_list"))
	if len(proxies) == 0 {
		return ""
	}
	p.mu.Lock()
	proxy := proxies[p.pxIdx%len(proxies)]
	p.pxIdx++
	p.mu.Unlock()
	return proxyutil.WithBestGoTaskSession(proxy)
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// markBadDomain 记一次域名被拒。
func (p *Producer) markBadDomain(email string) {
	domain := mailDomain(email)
	if domain == "" {
		return
	}
	p.mu.Lock()
	p.badDomains[domain]++
	p.mu.Unlock()
}

// blockedDomains 返回本次生产要跳过的邮箱域名。
func (p *Producer) blockedDomains() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.badDomains))
	for domain, n := range p.badDomains {
		if n >= badDomainLimit {
			out = append(out, domain)
		}
	}
	return out
}

// excludeDomains 给取号查询加上「排除这些邮箱域名」的条件。
func excludeDomains(q *gorm.DB, domains []string) *gorm.DB {
	for _, domain := range domains {
		q = q.Where("email NOT LIKE ?", "%@"+domain)
	}
	return q
}

func mailDomain(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return ""
}

