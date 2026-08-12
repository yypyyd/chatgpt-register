package grokproducer

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/grokoauth"
	"chatgpt-register/internal/grokreg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxyutil"

	"gorm.io/gorm"
)

const (
	codeWaitTimeout  = 10 * time.Minute
	codePollTimeout  = 4 * time.Minute
	codePollInterval = 5 * time.Second
	maxLogBytes      = 64 * 1024

	// defaultMaxConcurrency 未配置并发时的默认值：逐个开工。批量注册时多个
	// 有头浏览器 + Turnstile 令牌池同时抢 CPU 会互相超时，串行最稳。
	// 可用设置页「最大并发数」(max_concurrency) 调大。
	defaultMaxConcurrency = 1

	// defaultRetryCooldownMin 注册失败后重试同一邮箱前的等待分钟数，避免连续重试
	// 撞风控。可在系统设置 retry_cooldown_min 覆盖，0 = 不冷却。
	defaultRetryCooldownMin = 30

	// defaultMailboxIntervalMin 同一个母邮箱两次注册之间的最小间隔分钟数：刚注册完
	// 就接着开下一个裂变，两封验证码邮件容易互相干扰。
	// 可在系统设置 mailbox_interval_min 覆盖，0 = 不限。
	defaultMailboxIntervalMin = 5
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
	active  int // 当前真正在跑（已获得并发槽位）的任务数
	pxIdx   int
	// topUpCancel 非空表示补任务循环在跑，StopAll 用它停掉补任务。
	topUpCancel context.CancelFunc
	runTarget   int    // 本次生产的目标数量
	runTracked  []uint // 本次生产建过的任务 id

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

// StartFromAccounts 从账号管理 / 邮箱管理取号开工，并在后台补任务：注册失败的邮箱
// 过了「失败重试冷却」(retry_cooldown_min，默认 30 分钟) 会被重新拿来重试，直到本次
// 拿到 count 个已注册的账号，或者再也没有可用邮箱。
func (p *Producer) StartFromAccounts(count int) ([]models.GrokRegistration, error) {
	if count < 1 {
		return nil, fmt.Errorf("数量必须 >= 1")
	}
	// 首批只按并发数取号，跑完一条再补一条，避免一次把上百个邮箱全标成注册中。
	started, cooling, err := p.claimTargets(minInt(count, p.maxConcurrency()))
	if err != nil {
		return nil, err
	}
	if len(started) == 0 {
		if cooling {
			return nil, fmt.Errorf("可重试的邮箱都还在失败冷却中，请稍后再试")
		}
		return nil, fmt.Errorf("账号管理和邮箱管理里都没有可用于 Grok 注册的账号")
	}
	p.beginRun(count, started)
	for _, reg := range started {
		go p.run(reg.ID)
	}
	go p.topUp(count, started)
	return started, nil
}

// claimTargets 取最多 count 个可注册的邮箱并置为 registering。
// cooling=true 表示暂时无号可领、但等一会儿会有（失败地址在冷却、或某邮箱刚跑过）。
func (p *Producer) claimTargets(count int) ([]models.GrokRegistration, bool, error) {
	cutoff := time.Now().Add(-p.retryCooldown())
	// 已注册/在跑的邮箱要排除；失败的邮箱过了冷却才允许重试。
	blocked := p.db.Model(&models.GrokRegistration{}).
		Select("email").
		Where("status <> ? OR updated_at > ?", "register_failed", cutoff)
	// 同一个母邮箱同时只跑一个，且两次注册之间留出间隔：刚注册完就接着开下一个
	// 裂变，两封验证码邮件挤在一起容易读错/读不到。
	busyCutoff := time.Now().Add(-p.mailboxInterval())
	busyMailboxes := p.db.Model(&models.GrokRegistration{}).
		Select("mailbox_id").
		Where("mailbox_id <> 0").
		Where("status IN ? OR updated_at > ?", []string{"registering", "waiting_code"}, busyCutoff)

	started := make([]models.GrokRegistration, 0, count)
	// 同一批里也要按母邮箱去重：+001/+002 属于同一个邮箱，一起开会撞验证码。
	claimed := map[uint]bool{}
	var accounts []models.Registration
	if err := p.db.
		Where("status = ? AND mailbox_id <> 0", "registered").
		Where("email NOT IN (?)", blocked).
		Where("mailbox_id NOT IN (?)", busyMailboxes).
		Order("id asc").
		Limit(count * 20).
		Find(&accounts).Error; err != nil {
		return nil, false, err
	}
	for _, acc := range accounts {
		if len(started) >= count {
			break
		}
		if claimed[acc.MailboxID] {
			continue
		}
		reg, err := p.claimOne(acc.Email, acc.MailboxID,
			fmt.Sprintf("来源: ChatGPT账号 #%d，自动读取验证码", acc.ID))
		if err != nil {
			return started, false, err
		}
		claimed[acc.MailboxID] = true
		started = append(started, *reg)
	}
	if len(started) < count {
		var mailboxes []models.Mailbox
		if err := p.db.
			Where("status = ?", "verified").
			Where("email NOT IN (?)", blocked).
			Where("id NOT IN (?)", busyMailboxes).
			Order("id asc").
			Limit(count * 20).
			Find(&mailboxes).Error; err != nil {
			return started, false, err
		}
		for _, mb := range mailboxes {
			if len(started) >= count {
				break
			}
			if claimed[mb.ID] {
				continue
			}
			reg, err := p.claimOne(mb.Email, mb.ID,
				fmt.Sprintf("来源: 邮箱管理 #%d，自动读取验证码", mb.ID))
			if err != nil {
				return started, false, err
			}
			claimed[mb.ID] = true
			started = append(started, *reg)
		}
	}
	if len(started) > 0 {
		return started, false, nil
	}
	return started, p.hasCoolingFailure(cutoff) || p.hasBusyMailbox(busyCutoff), nil
}

// claimOne 把一个邮箱置为 registering：已有记录就复用（重试同一条），否则新建。
func (p *Producer) claimOne(email string, mailboxID uint, note string) (*models.GrokRegistration, error) {
	reg := models.GrokRegistration{
		Email:     email,
		MailboxID: mailboxID,
		Password:  grokreg.GenPassword(16),
		Status:    "registering",
		Note:      note,
	}
	var existing models.GrokRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		existing.MailboxID = mailboxID
		existing.Status = "registering"
		existing.Shipped = false
		existing.Password = reg.Password
		existing.Note = note
		existing.AuthData = ""
		existing.Shot = nil
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

// hasCoolingFailure 是否还有失败邮箱只是没到冷却时间（此时值得等，而不是收工）。
func (p *Producer) hasCoolingFailure(cutoff time.Time) bool {
	var n int64
	p.db.Model(&models.GrokRegistration{}).
		Where("status = ? AND updated_at > ?", "register_failed", cutoff).
		Count(&n)
	return n > 0
}

// hasBusyMailbox 是否有邮箱只是刚跑过、等间隔到了就能继续。
func (p *Producer) hasBusyMailbox(cutoff time.Time) bool {
	var n int64
	p.db.Model(&models.GrokRegistration{}).
		Where("mailbox_id <> 0 AND updated_at > ?", cutoff).
		Count(&n)
	return n > 0
}

// mailboxInterval 同一邮箱两次注册之间的最小间隔，跟设置页 mailbox_interval_min，
// 0 = 不限。
func (p *Producer) mailboxInterval() time.Duration {
	return time.Duration(p.settingInt("mailbox_interval_min", defaultMailboxIntervalMin)) * time.Minute
}

// retryCooldown 失败地址的重试冷却，跟设置页 retry_cooldown_min，0 = 不冷却。
func (p *Producer) retryCooldown() time.Duration {
	return time.Duration(p.settingInt("retry_cooldown_min", defaultRetryCooldownMin)) * time.Minute
}

// settingInt 读一个非负整型设置，未设置或非法时用默认值。
func (p *Producer) settingInt(key string, def int) int {
	raw := strings.TrimSpace(p.getSetting(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// topUp 边跑边补：始终让在跑任务数维持在并发上限，直到本次生产拿到 count 个已注册
// 账号。失败的邮箱在冷却期内不会被重试，此时循环等待而不是提前收工。
func (p *Producer) topUp(count int, started []models.GrokRegistration) {
	ctx, ok := p.beginTopUp()
	if !ok { // 已有补任务循环在跑，交给它继续
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
				continue // 等在跑的任务出结果，失败了再补
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
				continue // 等失败地址过冷却
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

// beginRun 记录本次生产的目标与首批任务，供进度里的「待生产」计算剩余数量。
func (p *Producer) beginRun(count int, started []models.GrokRegistration) {
	p.mu.Lock()
	p.runTarget = count
	p.runTracked = make([]uint, 0, count)
	for _, reg := range started {
		p.runTracked = append(p.runTracked, reg.ID)
	}
	p.mu.Unlock()
}

// trackRun 把补出来的任务并入本次生产。
func (p *Producer) trackRun(regs []models.GrokRegistration) {
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

// beginTopUp 保证同一时间只有一个补任务循环；返回的 ctx 在 StopAll 时取消。
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

// countRegistered 本次生产里已经注册成功的数量。
func (p *Producer) countRegistered(ids []uint) int {
	if len(ids) == 0 {
		return 0
	}
	var n int64
	p.db.Model(&models.GrokRegistration{}).
		Where("id IN ? AND status = ?", ids, "registered").
		Count(&n)
	return int(n)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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

// StopAll 请求停止所有在跑的 Grok 注册任务，并停掉失败重试的补任务循环。
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

// Progress 生产进度快照，供 /api/grok/produce/status 展示。
type Progress struct {
	Running    bool `json:"running"`
	Pending    int  `json:"pending"`     // 待生产
	RunningNum int  `json:"running_num"` // 在跑
	Registered int  `json:"registered"`  // 已注册
	Failed     int  `json:"failed"`      // 注册失败
}

// Snapshot 返回 Grok 生产进度：在跑数取自当前运行的任务，其余按库中状态统计。
func (p *Producer) Snapshot() Progress {
	p.mu.Lock()
	runningNum := len(p.cancel)
	topUp := p.topUpCancel != nil
	p.mu.Unlock()

	count := func(statuses ...string) int {
		var n int64
		p.db.Model(&models.GrokRegistration{}).Where("status IN ?", statuses).Count(&n)
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

// pendingRemaining 本次生产还差多少个：目标 − 已成功 − 在跑。没有在跑的生产时为 0。
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

	var reg models.GrokRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return
	}

	// 并发闸门：并发已满时排队等待，避免多个有头浏览器同时抢 CPU 互相超时。
	if !p.acquireSlot(ctx, id) {
		p.appendLog(id, "已取消（排队等待空闲注册槽位时被停止）")
		p.db.Model(&models.GrokRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}
	defer p.releaseSlot()

	p.appendLog(id, "开始 Grok 邮箱注册")
	// since 在获得槽位后再取，避免排队期间旧验证码被误读。
	since := time.Now().Add(-30 * time.Second)

	in := grokreg.Input{
		Email:    reg.Email,
		Password: reg.Password,
		Proxy:    p.nextProxy(),
		// Match the reference project: Grok registration is headed by default.
		// A dedicated opt-in setting can still enable headless for diagnostics.
		Headless: p.getSetting("grok_headless") == "1",
		// 协议注册为默认路径：只借浏览器签发 Turnstile 令牌，拿到后立即退出、
		// 其余全走 HTTP/gRPC。设置 grok_engine=browser 可回退到旧的全程浏览器流程。
		Engine:              p.getSetting("grok_engine"),
		Impersonate:         p.getSetting("grok_impersonate"),
		ImpersonateFallback: p.getSetting("grok_impersonate_fallback"),
		FlareSolverrURL:     p.getSetting("grok_flaresolverr_url"),
		ClearanceProxy:      p.getSetting("grok_clearance_proxy"),
		ClearanceURLs:       p.getSetting("grok_clearance_urls"),
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

	p.mintOAuth(ctx, id, in.Proxy, reg.Email, res.AuthJSON)

	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.appendLog(id, "Grok 注册成功")
	p.db.Model(&models.GrokRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"status":    "registered",
		"auth_data": string(authBytes),
	})
}

// mintOAuth 注册成功后立刻用 sso 换一份 xAI Build OAuth 令牌，写进 auth JSON 的 oauth
// 字段：Sub2API 与 CPA 都只认 OAuth 令牌，趁注册用的代理和出口还在换成功率最高，
// 导出时也就不用再等。换不到只记日志，不影响注册结果（导出时还会补换）。
func (p *Producer) mintOAuth(ctx context.Context, id uint, proxy, email string, auth map[string]any) {
	if auth == nil {
		return
	}
	sso := grokoauth.SSOFromAuth(auth)
	if sso == "" {
		return
	}
	p.appendLog(id, "正在换取 Grok OAuth 令牌（Sub2API / CPA 导出用）")
	info, err := grokoauth.ConvertSSO(ctx, proxy, sso)
	if err != nil {
		p.appendLog(id, "换取 OAuth 令牌失败，导出时会重试: "+err.Error())
		return
	}
	auth["oauth"] = grokoauth.Credentials(info, email)
	p.appendLog(id, "已换取 Grok OAuth 令牌")
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

// maxConcurrency 读取并发上限：跟设置页「最大并发数」(max_concurrency)，
// 未设置则默认 1，最小为 1。
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

// acquireSlot 阻塞直到并发未满（获得槽位返回 true）或 ctx 取消（返回 false）。
// 限额从设置动态读取，改大后新任务无需重启即可生效。
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

// releaseSlot 释放一个并发槽位。
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
	// Grok 跟随设置页上的全局代理开关与代理列表，不再有独立的 Grok 开关。
	enabled := strings.TrimSpace(p.getSetting("proxy_enabled"))
	raw := p.getSetting("proxy_list")
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

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
