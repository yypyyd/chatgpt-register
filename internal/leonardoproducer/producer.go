package leonardoproducer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/leonardoreg"
	"chatgpt-register/internal/livecheck"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/prodcore"

	"gorm.io/gorm"
)

const (
	codeWaitTimeout = 10 * time.Minute
	codePollTimeout = 4 * time.Minute
	// 收码轮询间隔：验证码邮件通常几秒内到，间隔越大平均多等半个间隔。
	codePollInterval = 2 * time.Second
	maxLogBytes      = 64 * 1024

	// defaultRetryCooldownMin 注册失败后重试同一邮箱前的等待分钟数，避免连续重试
	// 撞风控。可在系统设置 retry_cooldown_min 覆盖，0 = 不冷却。
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
	runTarget   int    // 本次生产的目标数量
	runTracked  []uint // 本次生产建过的任务 id
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

func (p *Producer) Start(email, note string) (*models.LeonardoRegistration, error) {
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

	var existing models.LeonardoRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		if existing.Status == "registering" || existing.Status == "waiting_code" {
			return nil, fmt.Errorf("该邮箱的 Leonardo 注册正在进行中")
		}
		existing.Status = "registering"
		existing.Note = note
		existing.MailboxID = mailboxID
		existing.Password = leonardoreg.GenPassword(16)
		existing.AuthData = ""
		existing.Shot = nil
		existing.Shipped = false
		if err := p.db.Save(&existing).Error; err != nil {
			return nil, err
		}
		go p.run(existing.ID)
		return &existing, nil
	}

	reg := models.LeonardoRegistration{Email: email, MailboxID: mailboxID, Password: leonardoreg.GenPassword(16), Status: "registering", Note: note}
	if err := p.db.Create(&reg).Error; err != nil {
		return nil, err
	}
	go p.run(reg.ID)
	return &reg, nil
}

// StartFromAccounts 从账号管理 / 邮箱管理取号开工，并在后台补任务：注册失败的邮箱
// 过了「失败重试冷却」(retry_cooldown_min，默认 30 分钟) 会被重新拿来重试，直到本次
// 拿到 count 个已注册的账号，或者再也没有可用邮箱。
func (p *Producer) StartFromAccounts(count int) ([]models.LeonardoRegistration, error) {
	if count < 1 {
		return nil, fmt.Errorf("数量必须 >= 1")
	}
	// 首批只按并发数取号，跑完一条再补一条，避免一次把上百个邮箱全标成注册中。
	started, cooling, err := p.claimTargets(min(count, p.MaxConcurrency()))
	if err != nil {
		return nil, err
	}
	if len(started) == 0 {
		if cooling {
			return nil, fmt.Errorf("可重试的邮箱都还在失败冷却中，请稍后再试")
		}
		return nil, fmt.Errorf("账号管理和邮箱管理里都没有可用于 Leonardo 注册的账号")
	}
	p.beginRun(count, started)
	for _, reg := range started {
		go p.run(reg.ID)
	}
	go p.topUp(count, started)
	return started, nil
}

// claimTargets 取最多 count 个可注册的邮箱并置为 registering。
// cooling=true 表示还有失败邮箱可重试、只是没到冷却时间。
func (p *Producer) claimTargets(count int) ([]models.LeonardoRegistration, bool, error) {
	cutoff := time.Now().Add(-p.retryCooldown())
	// 已注册/在跑的邮箱要排除；失败的邮箱过了冷却才允许重试。
	blocked := p.db.Model(&models.LeonardoRegistration{}).
		Select("email").
		Where("status <> ? OR updated_at > ?", "register_failed", cutoff)

	started := make([]models.LeonardoRegistration, 0, count)
	// 优先用已注册 ChatGPT 账号（其邮箱已在邮箱池且可读验证码）。
	// 只取母号：裂变号是 +别名 邮箱，Leonardo 视作同一邮箱，不能用来注册。
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
	// 不足则从已验证邮箱池补齐。
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

// claimOne 把一个邮箱置为 registering：已有记录就复用（重试同一条），否则新建。
func (p *Producer) claimOne(email string, mailboxID uint, note string) (*models.LeonardoRegistration, error) {
	reg := models.LeonardoRegistration{
		Email:     email,
		MailboxID: mailboxID,
		Password:  leonardoreg.GenPassword(16),
		Status:    "registering",
		Note:      note,
	}
	var existing models.LeonardoRegistration
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
	p.db.Model(&models.LeonardoRegistration{}).
		Where("status = ? AND updated_at > ?", "register_failed", cutoff).
		Count(&n)
	return n > 0
}

// retryCooldown 失败地址的重试冷却，跟设置页 retry_cooldown_min，0 = 不冷却。
func (p *Producer) retryCooldown() time.Duration {
	return p.SettingMinutes("retry_cooldown_min", defaultRetryCooldownMin)
}

// topUp 边跑边补：始终让在跑任务数维持在并发上限，直到本次生产拿到 count 个已注册
// 账号。失败的邮箱在冷却期内不会被重试，此时循环等待而不是提前收工。
func (p *Producer) topUp(count int, started []models.LeonardoRegistration) {
	ctx, ok := p.beginTopUp()
	if !ok { // 已有补任务循环在跑，交给它继续
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
				continue // 等在跑的任务出结果，失败了再补
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
func (p *Producer) beginRun(count int, started []models.LeonardoRegistration) {
	p.mu.Lock()
	p.runTarget = count
	p.runTracked = make([]uint, 0, count)
	for _, reg := range started {
		p.runTracked = append(p.runTracked, reg.ID)
	}
	p.mu.Unlock()
}

// trackRun 把补出来的任务并入本次生产。
func (p *Producer) trackRun(regs []models.LeonardoRegistration) {
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
	p.db.Model(&models.LeonardoRegistration{}).
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

// StopAll 请求停止所有在跑的 Leonardo 注册任务，并停掉失败重试的补任务循环。
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

// Progress 生产进度快照，供 /api/leonardo/produce/status 展示。
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
		p.db.Model(&models.LeonardoRegistration{}).Where("status IN ?", statuses).Count(&n)
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

	var reg models.LeonardoRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return
	}

	if !p.acquireSlot(ctx, id) {
		p.appendLog(id, "已取消（排队等待空闲注册槽位时被停止）")
		p.db.Model(&models.LeonardoRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}
	defer p.ReleaseSlot()

	p.appendLog(id, "开始 Leonardo 邮箱注册")
	since := time.Now().Add(-30 * time.Second)

	in := leonardoreg.Input{
		Email:    reg.Email,
		Password: reg.Password,
		Proxy:    p.NextProxy(),
		Headless: p.SettingOn("leonardo_headless"),
		// 出口 IP 探测默认关闭以提速；需排障时置 leonardo_egress_check=1。
		EgressCheck: p.SettingOn("leonardo_egress_check"),
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
			p.db.Model(&models.LeonardoRegistration{}).Where("id = ?", id).Update("shot", png)
		},
	}

	res, err := leonardoreg.Register(ctx, in)
	if err != nil {
		p.appendLog(id, "注册失败: "+err.Error())
		status := "register_failed"
		if errors.Is(err, leonardoreg.ErrEmailTaken) {
			// 邮箱已被注册是终态，标成 already_registered 后不再进冷却重试。
			status = "already_registered"
		}
		p.db.Model(&models.LeonardoRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": status,
			"note":   prodcore.Truncate(err.Error(), 500),
		})
		return
	}

	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.appendLog(id, "Leonardo 注册成功")
	p.db.Model(&models.LeonardoRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"status":    "registered",
		"auth_data": string(authBytes),
	})

	// 注册成功后立即用采集到的 Cookie 自检一次会话：会话无效说明这个号导出后
	// 不可用，自动标记为失效（不删号）。
	p.selfCheckLeonardoAlive(id, string(authBytes))
}

// selfCheckLeonardoAlive 用注册产出的 Cookie 探一次 Leonardo 会话，把结果写入
// alive 字段：alive=会话有效、dead=会话被拒、unknown=网络/限流不判死。
func (p *Producer) selfCheckLeonardoAlive(id uint, authData string) {
	cookies := livecheck.LeonardoCookiesFromAuthJSON(authData)
	if len(cookies) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result := livecheck.CheckLeonardo(ctx, []livecheck.LeonardoItem{{ID: id, Cookies: cookies}}, nil)
	alive := result[id]
	if alive == "" {
		alive = livecheck.StatusUnknown
	}
	now := time.Now()
	p.db.Model(&models.LeonardoRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"alive": alive, "alive_checked_at": now,
	})
	switch alive {
	case livecheck.StatusDead:
		p.appendLog(id, "自检：Leonardo 会话无效，已标记为失效")
	case livecheck.StatusAlive:
		p.appendLog(id, "自检：Leonardo 会话有效，账号可用")
	default:
		p.appendLog(id, "自检：Leonardo 会话检测结果未知（网络/限流等），未改判")
	}
}

func (p *Producer) waitManualCode(ctx context.Context, id uint) (string, error) {
	ch := make(chan string, 1)
	p.mu.Lock()
	p.waiters[id] = ch
	p.mu.Unlock()
	p.db.Model(&models.LeonardoRegistration{}).Where("id = ?", id).Update("status", "waiting_code")

	timer := time.NewTimer(codeWaitTimeout)
	defer timer.Stop()
	select {
	case code := <-ch:
		p.db.Model(&models.LeonardoRegistration{}).Where("id = ?", id).Update("status", "registering")
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
	p.appendLog(id, "开始自动读取 Leonardo 邮件验证码")
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := p.mail.ListMessages(ctx, acc, 15)
		if err == nil {
			for _, m := range msgs {
				if m.ReceivedAt.Before(since) || !looksLikeLeonardo(m) {
					continue
				}
				if code := extractLeonardoCode(m.Subject); code != "" {
					p.appendLog(id, "已从邮件标题读取验证码并自动提交")
					return code, nil
				}
				full, gerr := p.mail.GetMessage(ctx, acc, m.ID)
				if gerr != nil {
					continue
				}
				if code := extractLeonardoCode(full.Subject + " " + full.Text); code != "" {
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
	return "", fmt.Errorf("超时未收到 Leonardo 验证码邮件")
}

func extractLeonardoCode(s string) string {
	if code := digitCodeRe.FindStringSubmatch(s); code != nil {
		return code[1]
	}
	return ""
}

func looksLikeLeonardo(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "leonardo") ||
		strings.Contains(s, "verification code") ||
		strings.Contains(s, "confirmation code") ||
		strings.Contains(s, "verify your") ||
		strings.Contains(s, "one-time")
}

func (p *Producer) appendLog(id uint, line string) {
	var reg models.LeonardoRegistration
	if err := p.db.Select("log").First(&reg, id).Error; err != nil {
		return
	}
	p.db.Model(&models.LeonardoRegistration{}).Where("id = ?", id).
		Update("log", prodcore.AppendLogLine(reg.Log, line, maxLogBytes))
}

// acquireSlot 阻塞直到拿到并发槽位；ctx 取消时返回 false。
func (p *Producer) acquireSlot(ctx context.Context, id uint) bool {
	return p.AcquireSlot(ctx, func() {
		p.appendLog(id, "并发已满，排队等待空闲注册槽位")
	})
}
