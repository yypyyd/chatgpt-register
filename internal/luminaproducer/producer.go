package luminaproducer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/luminareg"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxyutil"

	"gorm.io/gorm"
)

const (
	codeWaitTimeout = 10 * time.Minute
	codePollTimeout = 4 * time.Minute
	// 收码轮询间隔：验证码邮件通常几秒内到，间隔越大平均多等半个间隔。
	codePollInterval = 2 * time.Second
	maxLogBytes      = 64 * 1024

	// defaultMaxConcurrency 未配置并发时的默认值：逐个开工。批量注册时多个
	// 有头浏览器同时抢 CPU 会互相超时，串行最稳。
	// 可用设置页「最大并发数」(max_concurrency) 调大。
	defaultMaxConcurrency = 1

	// defaultRetryCooldownMin 注册失败后重试同一邮箱前的等待分钟数，避免连续重试
	// 撞风控。可在系统设置 retry_cooldown_min 覆盖，0 = 不冷却。
	defaultRetryCooldownMin = 30

	// defaultRegisterSpacingSec 相邻两单注册的最小启动间隔秒数。实测 BytePlus 按注册
	// 数量给配额：猛冲能连过 50~70 单，之后封 20~30 分钟，平均约每分钟 2 单；把节奏
	// 压到这个间隔内基本不会再撞限流。可在系统设置 register_spacing_sec 覆盖，0 = 不错峰。
	defaultRegisterSpacingSec = 25

	// rateLimitCooldown 被限流的邮箱的重试冷却：限流不是这个邮箱的问题，别按失败
	// 罚它 30 分钟，短暂避让后放回池子继续用。
	rateLimitCooldown = 5 * time.Minute

	// maxRegisterSpacingSec 自适应错峰的间隔上限：BytePlus 配额吃紧时逐步拉长到
	// 这个值为止，避免无限变慢。
	maxRegisterSpacingSec = 180

	// rateLimitNote 限流失败记录的备注前缀，取号时据此走短冷却。
	rateLimitNote = "[限流]"
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
	// topUpCancel 非空表示补任务循环在跑，StopAll 用它停掉补任务。
	topUpCancel context.CancelFunc
	runTarget   int    // 本次生产的目标数量
	runTracked  []uint // 本次生产建过的任务 id
	// lastStart 上一单注册的开工时间，用于相邻两单错峰。
	lastStart time.Time
	// dynSpacing 自适应错峰的当前间隔：撞限流加倍拉长，注册成功逐步收窄，
	// 始终不低于 registerSpacing 的基础值。零值表示尚未调整过。
	dynSpacing time.Duration
}

func New(db *gorm.DB, mail *mailfetch.Client) *Producer {
	return &Producer{
		db:      db,
		mail:    mail,
		waiters: map[uint]chan string{},
		cancel:  map[uint]context.CancelFunc{},
	}
}

func (p *Producer) Start(email, note string) (*models.LuminaRegistration, error) {
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

	var existing models.LuminaRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		if existing.Status == "registering" || existing.Status == "waiting_code" {
			return nil, fmt.Errorf("该邮箱的 Lumina 注册正在进行中")
		}
		existing.Status = "registering"
		existing.Note = note
		existing.MailboxID = mailboxID
		existing.Password = luminareg.GenPassword(16)
		existing.AuthData = ""
		existing.Shot = nil
		existing.Shipped = false
		if err := p.db.Save(&existing).Error; err != nil {
			return nil, err
		}
		go p.run(existing.ID)
		return &existing, nil
	}

	reg := models.LuminaRegistration{Email: email, MailboxID: mailboxID, Password: luminareg.GenPassword(16), Status: "registering", Note: note}
	if err := p.db.Create(&reg).Error; err != nil {
		return nil, err
	}
	go p.run(reg.ID)
	return &reg, nil
}

// StartFromAccounts 从账号管理 / 邮箱管理取号开工，并在后台补任务：注册失败的邮箱
// 过了「失败重试冷却」(retry_cooldown_min，默认 30 分钟) 会被重新拿来重试，直到本次
// 拿到 count 个已注册的账号，或者再也没有可用邮箱。
func (p *Producer) StartFromAccounts(count int) ([]models.LuminaRegistration, error) {
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
		return nil, fmt.Errorf("账号管理和邮箱管理里都没有可用于 Lumina 注册的账号")
	}
	p.beginRun(count, started)
	for _, reg := range started {
		go p.run(reg.ID)
	}
	go p.topUp(count, started)
	return started, nil
}

// preferredDomainCond 圈出风控宽松的邮箱域：BytePlus 对 outlook.com / outlook.de
// 的注册接口风控明显更严（直接回 ErrorRateLimit），hotmail / live 同为微软邮箱却能
// 正常进滑块，所以先把它们用完，再回落到 outlook。
const preferredDomainCond = "email LIKE '%@hotmail.com' OR email LIKE '%@hotmail.co%' " +
	"OR email LIKE '%@live.com' OR email LIKE '%@live.co%'"

// claimTargets 取最多 count 个可注册的邮箱并置为 registering。
// cooling=true 表示还有失败邮箱可重试、只是没到冷却时间。
func (p *Producer) claimTargets(count int) ([]models.LuminaRegistration, bool, error) {
	cutoff := time.Now().Add(-p.retryCooldown())
	rlCutoff := time.Now().Add(-rateLimitCooldown)
	if rlCutoff.Before(cutoff) {
		rlCutoff = cutoff
	}
	// 已注册/在跑的邮箱要排除；失败的邮箱过了冷却才允许重试，其中被限流的走短冷却。
	blocked := p.db.Model(&models.LuminaRegistration{}).
		Select("email").
		Where("status <> ? OR updated_at > (CASE WHEN note LIKE ? THEN ? ELSE ? END)",
			"register_failed", rateLimitNote+"%", rlCutoff, cutoff)

	// 失败过的邮箱（如被限流）优先级放到最后：第一轮只取从没失败过的新邮箱，
	// 新邮箱用完了，第二轮才用过了冷却的失败邮箱补齐。
	everFailed := p.db.Model(&models.LuminaRegistration{}).
		Select("email").
		Where("status = ?", "register_failed")

	started := make([]models.LuminaRegistration, 0, count)
	// preferredOnly 那一轮只取 hotmail / live，用完才允许取 outlook。
	claim := func(skipFailed, preferredOnly bool) error {
		// 优先用已注册 ChatGPT 账号（其邮箱已在邮箱池且可读验证码）。
		// 只取母号：裂变号是 +别名 邮箱，BytePlus 视作同一邮箱，不能用来注册。
		accQ := p.db.
			Where("status = ? AND mailbox_id <> 0 AND is_mother = ?", "registered", true).
			Where("email NOT IN (?)", blocked)
		if skipFailed {
			accQ = accQ.Where("email NOT IN (?)", everFailed)
		}
		if preferredOnly {
			accQ = accQ.Where(preferredDomainCond)
		}
		var accounts []models.Registration
		if err := accQ.Order("id asc").Limit(count - len(started)).Find(&accounts).Error; err != nil {
			return err
		}
		for _, acc := range accounts {
			reg, err := p.claimOne(acc.Email, acc.MailboxID,
				fmt.Sprintf("来源: ChatGPT账号 #%d，自动读取验证码", acc.ID))
			if err != nil {
				return err
			}
			started = append(started, *reg)
		}
		// 不足则从已验证邮箱池补齐。
		if len(started) >= count {
			return nil
		}
		mbQ := p.db.
			Where("status = ?", "verified").
			Where("email NOT IN (?)", blocked)
		if skipFailed {
			mbQ = mbQ.Where("email NOT IN (?)", everFailed)
		}
		if preferredOnly {
			mbQ = mbQ.Where(preferredDomainCond)
		}
		var mailboxes []models.Mailbox
		if err := mbQ.Order("id asc").Limit(count - len(started)).Find(&mailboxes).Error; err != nil {
			return err
		}
		for _, mb := range mailboxes {
			reg, err := p.claimOne(mb.Email, mb.ID,
				fmt.Sprintf("来源: 邮箱管理 #%d，自动读取验证码", mb.ID))
			if err != nil {
				return err
			}
			started = append(started, *reg)
		}
		return nil
	}
	for _, skipFailed := range []bool{true, false} {
		for _, preferredOnly := range []bool{true, false} {
			if len(started) >= count {
				break
			}
			if err := claim(skipFailed, preferredOnly); err != nil {
				return started, false, err
			}
		}
	}
	if len(started) > 0 {
		return started, false, nil
	}
	return started, p.hasCoolingFailure(cutoff), nil
}

// claimOne 把一个邮箱置为 registering：已有记录就复用（重试同一条），否则新建。
func (p *Producer) claimOne(email string, mailboxID uint, note string) (*models.LuminaRegistration, error) {
	reg := models.LuminaRegistration{
		Email:     email,
		MailboxID: mailboxID,
		Password:  luminareg.GenPassword(16),
		Status:    "registering",
		Note:      note,
	}
	var existing models.LuminaRegistration
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
	p.db.Model(&models.LuminaRegistration{}).
		Where("status = ? AND updated_at > ?", "register_failed", cutoff).
		Count(&n)
	return n > 0
}

// retryCooldown 失败地址的重试冷却，跟设置页 retry_cooldown_min，0 = 不冷却。
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

// registerSpacing 相邻两单注册的最小启动间隔，跟设置页 register_spacing_sec，0 = 不错峰。
func (p *Producer) registerSpacing() time.Duration {
	raw := strings.TrimSpace(p.getSetting("register_spacing_sec"))
	sec := defaultRegisterSpacingSec
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			sec = n
		}
	}
	return time.Duration(sec) * time.Second
}

// effectiveSpacing 当前生效的错峰间隔：取自适应间隔与基础间隔的较大者。
func (p *Producer) effectiveSpacing() time.Duration {
	base := p.registerSpacing()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dynSpacing > base {
		return p.dynSpacing
	}
	return base
}

// bumpSpacing 撞限流后把错峰间隔加倍（不超过上限），让节奏自动降到配额线下。
func (p *Producer) bumpSpacing(id uint) {
	base := p.registerSpacing()
	if base <= 0 {
		return // 用户显式关掉了错峰，不自作主张
	}
	p.mu.Lock()
	cur := p.dynSpacing
	if cur < base {
		cur = base
	}
	next := cur * 2
	if max := maxRegisterSpacingSec * time.Second; next > max {
		next = max
	}
	p.dynSpacing = next
	p.mu.Unlock()
	if next != cur {
		p.appendLog(id, fmt.Sprintf("检测到限流，错峰间隔调整为 %d 秒", int(next.Seconds()+0.5)))
	}
}

// easeSpacing 注册成功后逐步收窄错峰间隔（每次减 5 秒），直到回到基础值。
func (p *Producer) easeSpacing() {
	base := p.registerSpacing()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dynSpacing <= base {
		p.dynSpacing = 0
		return
	}
	p.dynSpacing -= 5 * time.Second
	if p.dynSpacing < base {
		p.dynSpacing = base
	}
}

// waitSpacing 错峰：与上一单开工时间至少隔 effectiveSpacing，把平均注册频率压在
// BytePlus 的配额线下。返回 false 表示等待期间被停止。
func (p *Producer) waitSpacing(ctx context.Context, id uint) bool {
	gap := p.effectiveSpacing()
	if gap <= 0 {
		return true
	}
	logged := false
	for {
		p.mu.Lock()
		wait := time.Until(p.lastStart.Add(gap))
		if wait <= 0 {
			p.lastStart = time.Now()
			p.mu.Unlock()
			return true
		}
		p.mu.Unlock()
		if !logged {
			p.appendLog(id, fmt.Sprintf("错峰等待 %d 秒后开工（避免撞 BytePlus 注册配额）", int(wait.Seconds()+0.5)))
			logged = true
		}
		if wait > time.Second {
			wait = time.Second
		}
		if !sleepCtx(ctx, wait) {
			return false
		}
	}
}

// topUp 边跑边补：始终让在跑任务数维持在并发上限，直到本次生产拿到 count 个已注册
// 账号。失败的邮箱在冷却期内不会被重试，此时循环等待而不是提前收工。
func (p *Producer) topUp(count int, started []models.LuminaRegistration) {
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
func (p *Producer) beginRun(count int, started []models.LuminaRegistration) {
	p.mu.Lock()
	p.runTarget = count
	p.runTracked = make([]uint, 0, count)
	for _, reg := range started {
		p.runTracked = append(p.runTracked, reg.ID)
	}
	p.mu.Unlock()
}

// trackRun 把补出来的任务并入本次生产。
func (p *Producer) trackRun(regs []models.LuminaRegistration) {
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
	p.db.Model(&models.LuminaRegistration{}).
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

// StopAll 请求停止所有在跑的 Lumina 注册任务，并停掉失败重试的补任务循环。
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

// Progress 生产进度快照，供 /api/lumina/produce/status 展示。
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
		p.db.Model(&models.LuminaRegistration{}).Where("status IN ?", statuses).Count(&n)
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

	var reg models.LuminaRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return
	}

	if !p.acquireSlot(ctx, id) {
		p.appendLog(id, "已取消（排队等待空闲注册槽位时被停止）")
		p.db.Model(&models.LuminaRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}
	defer p.releaseSlot()

	if !p.waitSpacing(ctx, id) {
		p.appendLog(id, "已取消（错峰等待时被停止）")
		p.db.Model(&models.LuminaRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}

	p.appendLog(id, "开始 Lumina 邮箱注册")

	// 与 GPT 生产一致：bestgo 住宅代理（带 -session-）可换出口 IP，被限流或
	// 滑块不过时换新 session(=新 IP) 重试，最多 maxRotateAttempts 次；
	// 直连/固定出口无法换 IP，失败直接进冷却。
	const maxRotateAttempts = 3
	var res *luminareg.Result
	var err error
	for attempt := 1; attempt <= maxRotateAttempts; attempt++ {
		proxy := p.nextProxy() // 每次调用都会挂新的住宅 session
		canRotate := strings.Contains(proxy, "-session-")
		if attempt > 1 {
			p.appendLog(id, fmt.Sprintf("♻ 更换住宅 IP 后第 %d/%d 次重试", attempt, maxRotateAttempts))
		}
		since := time.Now().Add(-30 * time.Second)

		in := luminareg.Input{
			Email:    reg.Email,
			Password: reg.Password,
			Proxy:    proxy,
			Log: func(f string, a ...any) {
				p.appendLog(id, fmt.Sprintf(f, a...))
			},
			WaitCode: func(ctx context.Context) (string, error) {
				if reg.MailboxID != 0 {
					return p.fetchCode(ctx, id, reg.MailboxID, since)
				}
				return p.waitManualCode(ctx, id)
			},
		}

		res, err = luminareg.Register(ctx, in)
		if err == nil {
			break
		}
		if errors.Is(err, luminareg.ErrRateLimited) {
			// 实测换出口 IP、换浏览器特征都拦：限流按注册数量算，原地重试白占并发槽。
			// 限流不是这个邮箱的问题，短冷却后放回池子继续用，不当成烧掉。
			p.bumpSpacing(id)
			p.appendLog(id, "注册未通过: "+err.Error()+"，先换下一个邮箱，本邮箱短暂避让后放回池子")
			break
		}
		if canRotate && attempt < maxRotateAttempts &&
			(errors.Is(err, luminareg.ErrCaptchaFailed) || errors.Is(err, luminareg.ErrRegionBlocked)) {
			p.appendLog(id, "注册未通过: "+err.Error())
			continue
		}
		break
	}
	if err != nil {
		p.appendLog(id, "注册失败: "+err.Error())
		status := "register_failed"
		note := err.Error()
		if errors.Is(err, luminareg.ErrEmailTaken) {
			// 邮箱已被注册是终态，标成 already_registered 后不再进冷却重试。
			status = "already_registered"
		}
		if errors.Is(err, luminareg.ErrRateLimited) {
			note = rateLimitNote + note
		}
		p.db.Model(&models.LuminaRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": status,
			"note":   truncateStr(note, 500),
		})
		return
	}

	p.easeSpacing()
	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.appendLog(id, "Lumina 注册成功")
	p.db.Model(&models.LuminaRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"status":    "registered",
		"auth_data": string(authBytes),
	})
}

func (p *Producer) waitManualCode(ctx context.Context, id uint) (string, error) {
	ch := make(chan string, 1)
	p.mu.Lock()
	p.waiters[id] = ch
	p.mu.Unlock()
	p.db.Model(&models.LuminaRegistration{}).Where("id = ?", id).Update("status", "waiting_code")

	timer := time.NewTimer(codeWaitTimeout)
	defer timer.Stop()
	select {
	case code := <-ch:
		p.db.Model(&models.LuminaRegistration{}).Where("id = ?", id).Update("status", "registering")
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
	p.appendLog(id, "开始自动读取 BytePlus 邮件验证码")
	// 邮箱服务（Graph）连续 503 等临时错误达到该时长即放弃并自动停用该邮箱。
	const mailTempErrGiveUp = 30 * time.Second
	var tempErrSince time.Time
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := p.mail.ListMessages(ctx, acc, 15)
		if err == nil {
			tempErrSince = time.Time{}
			for _, m := range msgs {
				if m.ReceivedAt.Before(since) || !looksLikeBytePlus(m) {
					continue
				}
				if code := extractCode(m.Subject); code != "" {
					p.appendLog(id, "已从邮件标题读取验证码并自动提交")
					return code, nil
				}
				full, gerr := p.mail.GetMessage(ctx, acc, m.ID)
				if gerr != nil {
					continue
				}
				if code := extractCode(full.Subject + " " + full.Text); code != "" {
					p.appendLog(id, "已从邮件正文读取验证码并自动提交")
					return code, nil
				}
			}
		} else {
			if errors.Is(err, mailfetch.ErrAuthTemporary) {
				if tempErrSince.IsZero() {
					tempErrSince = time.Now()
				} else if time.Since(tempErrSince) >= mailTempErrGiveUp {
					// 连续 503 多半是邮箱被微软风控，继续等也没用：自动停用该邮箱并跳过
					p.db.Model(&models.Mailbox{}).Where("id = ?", mb.ID).Update("status", "disabled")
					return "", fmt.Errorf("邮箱服务连续不可用超过 %v，已自动停用该邮箱并跳过: %v", mailTempErrGiveUp, err)
				}
			} else {
				tempErrSince = time.Time{}
			}
			p.appendLog(id, "读取邮件暂时失败，继续重试: "+err.Error())
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(codePollInterval):
		}
	}
	return "", fmt.Errorf("超时未收到 BytePlus 验证码邮件")
}

func extractCode(s string) string {
	if code := digitCodeRe.FindStringSubmatch(s); code != nil {
		return code[1]
	}
	return ""
}

func looksLikeBytePlus(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	// 同一邮箱可能同时收到其它平台的验证码邮件，先排除已知的非 BytePlus 来源。
	if strings.Contains(s, "openai") || strings.Contains(s, "chatgpt") {
		return false
	}
	return strings.Contains(s, "byteplus") ||
		strings.Contains(s, "volcengine") ||
		strings.Contains(s, "verification code") ||
		strings.Contains(s, "confirmation code") ||
		strings.Contains(s, "verify your") ||
		strings.Contains(s, "one-time")
}

func (p *Producer) appendLog(id uint, line string) {
	stamp := time.Now().Format("2006-01-02 15:04:05")
	var reg models.LuminaRegistration
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
	p.db.Model(&models.LuminaRegistration{}).Where("id = ?", id).Update("log", log)
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
