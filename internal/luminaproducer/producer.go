package luminaproducer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/luminareg"
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

	// defaultRegisterSpacingSec 相邻两单注册的最小启动间隔秒数，把节奏放缓一点，
	// 避免同一域名的配额被瞬间打空。可在系统设置 register_spacing_sec 覆盖，0 = 不错峰。
	defaultRegisterSpacingSec = 25

	// rateLimitCooldown 被限流的邮箱的重试冷却：限流不是这个邮箱的问题，别按失败
	// 罚它 30 分钟，短暂避让后放回池子继续用（真正的挡板是下面的域名冷却）。
	rateLimitCooldown = 5 * time.Minute

	// rateLimitNote 限流失败记录的备注前缀，取号时据此走短冷却。
	rateLimitNote = "[限流]"

	// domainCooldown 某个邮箱域名撞到 ErrorRateLimit 后整体停用的时长。线上实测
	// BytePlus 的限流按邮箱域名计，处在额度边缘时是间歇性拒绝（前后一分钟内有过
	// 有拒有放），彻底打满后封十几到二十几小时。短冷却既避免连续拿真邮箱去撞，
	// 又不会在间歇性限流时白白停工一小时。
	domainCooldown = 10 * time.Minute

	// candidateFetchLimit 取号时每类来源最多捞多少候选邮箱，再在内存里按域名轮询挑选。
	candidateFetchLimit = 500
)

var digitCodeRe = regexp.MustCompile(`\b(\d{6})\b`)

// excludedDomains 取号时始终跳过的邮箱域名：这些域名在 BytePlus 长期处于限流状态，
// 拿去注册只会白白消耗邮箱和验证码。
var excludedDomains = map[string]bool{
	"outlook.de": true,
}

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
	// lastStart 上一单注册的开工时间，用于相邻两单错峰。
	lastStart time.Time
	// domainBlockedUntil 撞限流的邮箱域名 → 解禁时间。
	domainBlockedUntil map[string]time.Time
}

func New(db *gorm.DB, mail *mailfetch.Client) *Producer {
	return &Producer{
		Core:               prodcore.New(db),
		db:                 db,
		mail:               mail,
		waiters:            map[uint]chan string{},
		cancel:             map[uint]context.CancelFunc{},
		domainBlockedUntil: map[string]time.Time{},
	}
}

// emailDomain 取邮箱 @ 后的域名（小写）。
func emailDomain(email string) string {
	if i := strings.LastIndex(email, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(email[i+1:]))
	}
	return ""
}

// blockDomain 把域名整体停用 domainCooldown，期间取号跳过该域名的所有邮箱。
func (p *Producer) blockDomain(domain string) time.Time {
	until := time.Now().Add(domainCooldown)
	p.mu.Lock()
	p.domainBlockedUntil[domain] = until
	p.mu.Unlock()
	return until
}

// domainBlocked 返回域名是否仍在冷却中，到点的顺手清掉。
func (p *Producer) domainBlocked(domain string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if until, ok := p.domainBlockedUntil[domain]; ok {
		if time.Now().Before(until) {
			return true
		}
		delete(p.domainBlockedUntil, domain)
	}
	return false
}

// blockedDomains 当前仍在冷却中的域名列表（已排序）。
func (p *Producer) blockedDomains() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]string, 0, len(p.domainBlockedUntil))
	for d, until := range p.domainBlockedUntil {
		if now.Before(until) {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// anyDomainBlocked 是否还有域名在冷却中（此时值得等，而不是收工）。
func (p *Producer) anyDomainBlocked() bool {
	return len(p.blockedDomains()) > 0
}

// domainUsable 取号前判断域名能否注册：长期限流的域名和冷却中的域名直接跳过。
func (p *Producer) domainUsable(domain string) bool {
	if domain == "" || excludedDomains[domain] {
		return false
	}
	return !p.domainBlocked(domain)
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
	started, cooling, err := p.claimTargets(min(count, p.MaxConcurrency()))
	if err != nil {
		return nil, err
	}
	if len(started) == 0 {
		if blocked := p.blockedDomains(); len(blocked) > 0 {
			return nil, fmt.Errorf("可用邮箱的域名（%s）都撞了 BytePlus 的域名注册配额，正在冷却；请补充其它域名的邮箱或稍后再试",
				strings.Join(blocked, ", "))
		}
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

// candidate 一个可用于注册的邮箱来源。
type candidate struct {
	email     string
	mailboxID uint
	note      string
}

// claimTargets 取最多 count 个可注册的邮箱并置为 registering。
// BytePlus 的注册配额按邮箱域名计，所以取号时按域名轮询交错，把用量摊到各个域名上；
// 长期限流的域名与冷却中的域名整体跳过。
// cooling=true 表示还有邮箱 / 域名可重试、只是没到冷却时间。
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
	claimed := map[string]bool{}
	for _, skipFailed := range []bool{true, false} {
		if len(started) >= count {
			break
		}
		cands, err := p.fetchCandidates(blocked, everFailed, skipFailed)
		if err != nil {
			return started, false, err
		}
		for _, c := range interleaveByDomain(cands) {
			if len(started) >= count {
				break
			}
			if claimed[c.email] || !p.domainUsable(emailDomain(c.email)) {
				continue
			}
			reg, err := p.claimOne(c.email, c.mailboxID, c.note)
			if err != nil {
				return started, false, err
			}
			claimed[c.email] = true
			started = append(started, *reg)
		}
	}
	if len(started) > 0 {
		return started, false, nil
	}
	return started, p.hasCoolingFailure(cutoff) || p.anyDomainBlocked(), nil
}

// fetchCandidates 捞出可注册的候选邮箱：优先已注册 ChatGPT 账号（其邮箱已在邮箱池且
// 可读验证码，只取母号——裂变号是 +别名 邮箱，BytePlus 视作同一邮箱），再补已验证邮箱池。
func (p *Producer) fetchCandidates(blocked, everFailed *gorm.DB, skipFailed bool) ([]candidate, error) {
	accQ := p.db.
		Where("status = ? AND mailbox_id <> 0 AND is_mother = ?", "registered", true).
		Where("email NOT IN (?)", blocked)
	if skipFailed {
		accQ = accQ.Where("email NOT IN (?)", everFailed)
	}
	var accounts []models.Registration
	if err := accQ.Order("id asc").Limit(candidateFetchLimit).Find(&accounts).Error; err != nil {
		return nil, err
	}
	cands := make([]candidate, 0, len(accounts))
	for _, acc := range accounts {
		cands = append(cands, candidate{
			email: acc.Email, mailboxID: acc.MailboxID,
			note: fmt.Sprintf("来源: ChatGPT账号 #%d，自动读取验证码", acc.ID),
		})
	}

	mbQ := p.db.
		Where("status = ?", "verified").
		Where("email NOT IN (?)", blocked)
	if skipFailed {
		mbQ = mbQ.Where("email NOT IN (?)", everFailed)
	}
	var mailboxes []models.Mailbox
	if err := mbQ.Order("id asc").Limit(candidateFetchLimit).Find(&mailboxes).Error; err != nil {
		return nil, err
	}
	for _, mb := range mailboxes {
		cands = append(cands, candidate{
			email: mb.Email, mailboxID: mb.ID,
			note: fmt.Sprintf("来源: 邮箱管理 #%d，自动读取验证码", mb.ID),
		})
	}
	return cands, nil
}

// interleaveByDomain 按域名分组后轮询交错（a1 b1 c1 a2 b2 ...），组内保持原顺序，
// 域名按首次出现顺序排列，让每一批取号尽量覆盖多个域名。
func interleaveByDomain(cands []candidate) []candidate {
	order := make([]string, 0, 4)
	groups := map[string][]candidate{}
	for _, c := range cands {
		d := emailDomain(c.email)
		if _, ok := groups[d]; !ok {
			order = append(order, d)
		}
		groups[d] = append(groups[d], c)
	}
	out := make([]candidate, 0, len(cands))
	for len(out) < len(cands) {
		for _, d := range order {
			if g := groups[d]; len(g) > 0 {
				out = append(out, g[0])
				groups[d] = g[1:]
			}
		}
	}
	return out
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
	return p.SettingMinutes("retry_cooldown_min", defaultRetryCooldownMin)
}

// registerSpacing 相邻两单注册的最小启动间隔，跟设置页 register_spacing_sec，0 = 不错峰。
func (p *Producer) registerSpacing() time.Duration {
	return time.Duration(p.SettingInt("register_spacing_sec", defaultRegisterSpacingSec)) * time.Second
}

// waitSpacing 错峰：与上一单开工时间至少隔 registerSpacing。返回 false 表示等待期间被停止。
func (p *Producer) waitSpacing(ctx context.Context, id uint) bool {
	gap := p.registerSpacing()
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
			p.appendLog(id, fmt.Sprintf("错峰等待 %d 秒后开工", int(wait.Seconds()+0.5)))
			logged = true
		}
		if wait > time.Second {
			wait = time.Second
		}
		if !prodcore.Sleep(ctx, wait) {
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
	defer p.ReleaseSlot()

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
		proxy := p.NextProxy() // 每次调用都会挂新的住宅 session
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
			// 限流按邮箱域名计，换 IP / 原地重试都没用：整个域名拉进冷却，让后续取号
			// 换其它域名；这个邮箱本身没问题，短冷却后放回池子。
			domain := emailDomain(reg.Email)
			until := p.blockDomain(domain)
			p.appendLog(id, fmt.Sprintf("注册未通过: %v；域名 @%s 已停用至 %s，后续取号改用其它域名的邮箱",
				err, domain, until.Format("15:04")))
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
			"note":   prodcore.Truncate(note, 500),
		})
		return
	}

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
	var reg models.LuminaRegistration
	if err := p.db.Select("log").First(&reg, id).Error; err != nil {
		return
	}
	p.db.Model(&models.LuminaRegistration{}).Where("id = ?", id).
		Update("log", prodcore.AppendLogLine(reg.Log, line, maxLogBytes))
}

// acquireSlot 阻塞直到拿到并发槽位；ctx 取消时返回 false。
func (p *Producer) acquireSlot(ctx context.Context, id uint) bool {
	return p.AcquireSlot(ctx, func() {
		p.appendLog(id, "并发已满，排队等待空闲注册槽位")
	})
}
