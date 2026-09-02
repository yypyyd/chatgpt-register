// Package producer 编排 ChatGPT 账号的批量"生产"。
//
// 规则：维持"母号 + 指定数量的裂变"——每个邮箱先注册主号(母号，用邮箱本身地址)，
// 母号成功后才用 plus addressing 别名(email+001@…)注册裂变子号，每个邮箱最多 1 + FissionCount 个账号。
//
// 目标数量 target 表示本次要成功产出的账号数。注册失败不计入成功，会自动补一个新任务
// 继续注册（"注册失败→注册数量-1→待生产+1"），直到达标或邮箱容量耗尽。
// 母号注册失败时该邮箱不会往下开裂变，下次仍优先重试母号。
//
// 验证码由 mailfetch 从邮箱自动读取，无需人工输入。
package producer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"gorm.io/gorm"
)

const (
	defaultMaxConcurrency = 3
	defaultFissionCount   = 5
	// 注册失败后的重试冷却（分钟），避免短时间内反复重试触发风控。
	// 可在系统设置 retry_cooldown_min 覆盖，0 = 不冷却。
	defaultRetryCooldownMin = 30
	// 同一个邮箱两次注册之间的最小间隔（分钟）：刚注册完就接着开下一个裂变，
	// 两封验证码邮件挤在一起容易读错/读不到。可在系统设置 mailbox_interval_min
	// 覆盖，0 = 不限。
	defaultMailboxIntervalMin = 5
	// registerAttemptTimeout 单次浏览器注册的墙钟上限：不加上限时任务卡死就一直处于 registering，
	// “停止”也要等它跑完才能结束。
	registerAttemptTimeout = 12 * time.Minute
	codePollTimeout        = 3 * time.Minute
	codePollInterval       = 5 * time.Second
	// 邮箱服务（Graph）连续 503 等临时错误达到该时长即放弃并自动停用该邮箱。
	mailTempErrGiveUp = 30 * time.Second
	maxLogLines       = 300
)

// openAI 验证码：6 位数字。
var codeRe = regexp.MustCompile(`\b(\d{6})\b`)

// Config 从系统设置装载的运行参数。
type Config struct {
	MaxConcurrency int
	FissionCount   int
	Headless       bool
	Proxies        []string      // 代理池，按账户轮转；空=直连
	RetryCooldown  time.Duration // 失败地址的重试冷却时间
	// MailboxInterval 同一邮箱两次注册之间的最小间隔，避免验证码邮件互相干扰
	MailboxInterval time.Duration
}

// Progress 生产进度快照，供 /api/produce/status 展示。
type Progress struct {
	Running    bool      `json:"running"`
	Target     int       `json:"target"`
	Pending    int       `json:"pending"`     // 待生产
	RunningNum int       `json:"running_num"` // 在跑
	Registered int       `json:"registered"`  // 已注册(成功)
	Failed     int       `json:"failed"`      // 注册失败(累计)
	Message    string    `json:"message"`
	Error      string    `json:"error"`
	Logs       []string  `json:"logs"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Producer 单例，管理一次生产任务的生命周期与进度。
type Producer struct {
	db   *gorm.DB
	mail *mailfetch.Client

	mu     sync.Mutex
	prog   Progress
	cancel context.CancelFunc

	claimMu  sync.Mutex      // 串行化任务领取
	inflight map[string]uint // email -> mailboxID，正在处理中的任务
	// failed 记录当前仍处于失败态的邮箱（重试成功后会移除）。
	// 只有最终没能注册成功的邮箱才计入失败数——中途重试失败不算。
	failed map[string]struct{}
	pxMu   sync.Mutex
	pxIdx  int
}

func New(db *gorm.DB, mail *mailfetch.Client) *Producer {
	return &Producer{db: db, mail: mail, inflight: map[string]uint{}, failed: map[string]struct{}{}}
}

// Start 启动一次生产（异步）。已在运行则返回错误。
func (p *Producer) Start(target int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prog.Running {
		return fmt.Errorf("生产任务已在运行中")
	}
	if target < 1 {
		return fmt.Errorf("生产数量必须 ≥ 1")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.inflight = map[string]uint{}
	p.failed = map[string]struct{}{}
	p.prog = Progress{Running: true, Target: target, Pending: target, Message: "初始化…", UpdatedAt: time.Now()}
	go p.run(ctx, target)
	return nil
}

// Stop 请求停止（在跑的账号会跑完，不再开新的）。
func (p *Producer) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
}

// Snapshot 返回进度副本。
func (p *Producer) Snapshot() Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := p.prog
	cp.Logs = append([]string(nil), p.prog.Logs...)
	return cp
}

func (p *Producer) run(ctx context.Context, target int) {
	defer func() {
		p.mu.Lock()
		p.prog.Running = false
		p.recalcLocked()
		p.prog.UpdatedAt = time.Now()
		p.mu.Unlock()
	}()

	cfg := p.loadConfig()
	p.logf("开始生产，目标 %d 个账号（每邮箱母号+%d 裂变，并发 %d）", target, cfg.FissionCount, cfg.MaxConcurrency)

	sem := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup

	for {
		if ctx.Err() != nil {
			p.logf("已手动停止")
			break
		}
		// 目标只统计"本次生产新产出 + 在跑"，不受库里历史已注册数影响，
		// 否则库里已有 N 个已注册时，再要求生产 ≤N 个会被误判为已完成而不跑。
		done := p.producedThisRun()
		running := p.inflightCount()
		if done+running >= target {
			if running == 0 {
				break
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}

		mb, email, isMother, ok, cooling := p.nextJob(cfg)
		if !ok {
			// 暂无可开的新任务：若还有在跑的，等它们（可能失败后要补）；
			// 若有地址在失败冷却期，等冷却结束后重试；否则容量耗尽
			if p.inflightCount() == 0 {
				if cooling {
					p.setMessage("失败地址冷却中，等待重试…")
					select {
					case <-ctx.Done():
					case <-time.After(15 * time.Second):
					}
					continue
				}
				p.logf("没有更多可用邮箱容量，本次已产出 %d 个", p.producedThisRun())
				break
			}
			time.Sleep(800 * time.Millisecond)
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(mb models.Mailbox, email string, isMother bool) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				p.releaseInflight(email)
				p.updateProgress()
			}()
			// 兜底：rod 的 MustXxx 会 panic，若不 recover 会连累整个进程崩溃（宕机）。
			// 把 panic 归一成一次注册失败，服务继续存活。
			defer func() {
				if r := recover(); r != nil {
					p.markFailed(email)
					msg := fmt.Sprintf("注册异常(panic): %v", r)
					// log 传空，保留 appendLog 实时写入的账号日志
					p.setRegistrationFailed(email, msg, "")
					p.logf("✗ %s %s\n%s", mask(email), msg, debug.Stack())
					p.updateProgress()
				}
			}()
			p.updateProgress()

			if err := p.produceOne(ctx, cfg, mb, email, isMother); err != nil {
				if errors.Is(err, codexreg.ErrAccountTaken) {
					// 账号停用不再重试，也不计为“失败”（属于跳过换号）
					p.logf("⚠ %s 停用（%v），不再重试，换下一个地址", mask(email), err)
				} else if errors.Is(err, codexreg.ErrTermsRejected) {
					// Terms of Use 拒绝为终态，不再重试也不计为“失败”，换下一个地址
					p.logf("⛔ %s Terms of Use 拒绝，标记为不可注册，换下一个地址", mask(email))
				} else {
					// 记为失败态；若后续重试成功会被 markSuccess 清除
					p.markFailed(email)
					p.logf("✗ %s 注册失败：%v", mask(email), err)
				}
			} else {
				p.markSuccess(email)
				p.incRegistered()
				p.logf("✓ %s 注册成功", mask(email))
			}
			p.updateProgress()
		}(mb, email, isMother)
	}

	wg.Wait()
	produced := p.producedThisRun()
	if ctx.Err() != nil {
		p.setMessage(fmt.Sprintf("已停止，本次成功 %d 个", produced))
	} else {
		p.setMessage(fmt.Sprintf("已完成，本次成功 %d 个", produced))
	}
}

// nextJob 领取下一个要注册的账号：先在所有邮箱补齐母号，再开裂变子号。
// 每个邮箱同一时刻只允许一个在跑任务（避免验证码串号，也保证母号先行）。
// 返回的 cooling 表示当前虽无可领任务，但有地址处于失败冷却期，稍后可重试。
func (p *Producer) nextJob(cfg Config) (models.Mailbox, string, bool, bool, bool) {
	p.claimMu.Lock()
	defer p.claimMu.Unlock()

	cooling := false
	var mailboxes []models.Mailbox
	if err := p.db.Where("status = ?", "verified").Order("id asc").Find(&mailboxes).Error; err != nil {
		return models.Mailbox{}, "", false, false, false
	}

	// Pass 1：母号（邮箱本身地址）未注册成功且该邮箱空闲 → 注册母号
	for _, mb := range mailboxes {
		if p.mailboxBusy(mb.ID) {
			continue
		}
		if p.mailboxUsedRecently(mb.ID, cfg.MailboxInterval) {
			cooling = true // 该邮箱刚跑过，等间隔到了再开下一个
			continue
		}
		if !p.isRegistered(mb.Email) {
			if p.failedRecently(mb.Email, cfg.RetryCooldown) {
				cooling = true // 母号刚失败过，冷却期内不重试
				continue
			}
			p.markInflight(mb.Email, mb.ID)
			return mb, mb.Email, true, true, false
		}
	}

	// Pass 2：母号已成功、该邮箱空闲、裂变未满 → 注册一个新的别名子号
	for _, mb := range mailboxes {
		if p.mailboxBusy(mb.ID) {
			continue
		}
		if p.mailboxUsedRecently(mb.ID, cfg.MailboxInterval) {
			cooling = true
			continue
		}
		if !p.isRegistered(mb.Email) {
			continue
		}
		if p.fissionCount(mb) >= cfg.FissionCount {
			continue
		}
		alias, aliasCooling := p.nextFissionEmail(mb.Email, cfg.RetryCooldown)
		if aliasCooling {
			cooling = true
		}
		if alias == "" {
			continue
		}
		p.markInflight(alias, mb.ID)
		return mb, alias, false, true, false
	}
	return models.Mailbox{}, "", false, false, cooling
}

// produceOne 完整生产一个账号：注册 ChatGPT → 保存账号凭据 → 入库。
func (p *Producer) produceOne(ctx context.Context, cfg Config, mb models.Mailbox, email string, isMother bool) error {
	password := codexreg.GenPassword(16)
	note := ""
	if !isMother {
		note = "裂变(" + mb.Email + ")"
	}
	p.upsert(models.Registration{
		Email: email, MailboxID: mb.ID, Password: password,
		Status: "registering", IsMother: isMother, Note: note,
	})

	var logMu sync.Mutex
	var logBuf strings.Builder
	var existing models.Registration
	if err := p.db.Select("log").Where("email = ?", email).First(&existing).Error; err == nil && strings.TrimSpace(existing.Log) != "" {
		logBuf.WriteString(existing.Log)
		if !strings.HasSuffix(existing.Log, "\n") {
			logBuf.WriteString("\n")
		}
		logBuf.WriteString(time.Now().Format("2006-01-02 15:04:05") + " --- 新一轮注册尝试 ---\n")
	}
	appendLog := func(line string) {
		logMu.Lock()
		logBuf.WriteString(time.Now().Format("2006-01-02 15:04:05") + " " + line + "\n")
		snapshot := logBuf.String()
		logMu.Unlock()
		// 实时写库，注册中的账号也能在弹窗里看到执行日志
		p.db.Model(&models.Registration{}).Where("email = ?", email).Update("log", snapshot)
	}

	// 出口 IP：轮转代理池取一个基础代理。若为 bestgo 住宅代理（可换 IP），
	// Terms of Use 拒绝时换新住宅 session(=新 IP) 重试，最多 maxTermsAttempts 次；
	// 直连/固定出口无法换 IP，Terms 拒绝直接标记 rejected，不空转重试。
	const maxTermsAttempts = 3
	baseProxy := p.nextProxy(cfg)
	canRotate := isRotatable(baseProxy)
	attempts := 1
	if canRotate {
		attempts = maxTermsAttempts
	}

	// 代理隧道瞬断（ERR_TUNNEL_CONNECTION_FAILED 等）换出口重试通常可恢复，
	// 独立于 Terms 重试计数，最多 maxNetRetries 次。
	const maxNetRetries = 2
	netRetry := 0
	var res *codexreg.Result
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		proxy := baseProxy
		if canRotate {
			proxy = rotateBestgoSession(baseProxy) // 每次尝试换新住宅 session = 新出口 IP
			if attempt > 1 {
				appendLog(fmt.Sprintf("♻ Terms of Use 拒绝，更换住宅 IP 后第 %d/%d 次重试", attempt, attempts))
			}
		}
		since := time.Now().Add(-30 * time.Second)
		in := codexreg.Input{
			Email:    email,
			Password: password,
			Proxy:    proxy,
			Headless: cfg.Headless,
			Log: func(f string, a ...any) {
				msg := fmt.Sprintf(f, a...)
				appendLog(msg)
				p.logf("%s", "  "+mask(email)+" "+msg)
			},
			FetchCode: func(ctx context.Context, after time.Time) (string, error) {
				if after.After(since) {
					return p.fetchCode(ctx, mb, after)
				}
				return p.fetchCode(ctx, mb, since)
			},
			SaveShot: func(png []byte) {
				p.db.Model(&models.Registration{}).Where("email = ?", email).Update("shot", png)
			},
		}
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, registerAttemptTimeout)
		res, err = codexreg.Register(attemptCtx, in)
		cancelAttempt()
		if err == nil {
			break
		}
		if errors.Is(err, codexreg.ErrTermsRejected) && canRotate && attempt < attempts {
			continue // 换 IP 再来一次
		}
		if isTransientNetErr(err) && netRetry < maxNetRetries && ctx.Err() == nil {
			netRetry++
			appendLog(fmt.Sprintf("♻ 网络/代理瞬断（%v），更换出口后第 %d/%d 次重试", err, netRetry, maxNetRetries))
			attempt-- // 不占用 Terms 重试次数
			continue
		}
		break
	}
	if err != nil {
		switch {
		case errors.Is(err, codexreg.ErrAccountTaken):
			appendLog("⚠ 停用（账号不存在或已被删除/停用），不再重试，换下一个地址继续")
			p.setRegistrationStatus(email, "already_registered", "停用："+err.Error(), logBuf.String())
			return err
		case errors.Is(err, codexreg.ErrTermsRejected):
			if canRotate {
				appendLog("⛔ 多次更换住宅 IP 仍被 Terms of Use 拒绝，标记为不可注册")
			} else {
				appendLog("⛔ Terms of Use 拒绝（直连/固定出口无法换 IP），标记为不可注册")
			}
			p.setRegistrationStatus(email, "register_rejected", "Terms of Use 拒绝："+err.Error(), logBuf.String())
			return err
		default:
			appendLog("✗ 失败: " + err.Error())
			p.setRegistrationFailed(email, err.Error(), logBuf.String())
			return err
		}
	}

	appendLog("✓ ChatGPT 注册成功（未执行 Agent Identity）")
	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.upsert(models.Registration{
		Email: email, MailboxID: mb.ID, Password: password,
		Status: "registered", IsMother: isMother, Note: note,
		AuthData: string(authBytes), AccountID: res.AccountID,
		UserID: res.UserID, PlanType: res.PlanType, Log: logBuf.String(),
	})
	return nil
}

// isTransientNetErr 网络/代理隧道瞬断类错误，换出口重试通常可恢复。
func isTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{
		"ERR_TUNNEL_CONNECTION_FAILED",
		"ERR_PROXY_CONNECTION_FAILED",
		"ERR_SOCKS_CONNECTION_FAILED",
		"ERR_CONNECTION_RESET",
		"ERR_CONNECTION_CLOSED",
		"ERR_CONNECTION_TIMED_OUT",
		"ERR_TIMED_OUT",
		"ERR_EMPTY_RESPONSE",
		"ERR_NAME_NOT_RESOLVED",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// fetchCode 轮询邮箱，从 OpenAI/ChatGPT 验证邮件里提取 6 位验证码。
func (p *Producer) fetchCode(ctx context.Context, mb models.Mailbox, since time.Time) (string, error) {
	acc := mailfetch.Account{Email: mb.Email, ClientID: mb.ClientID, RefreshToken: mb.RefreshToken}
	deadline := time.Now().Add(codePollTimeout)
	var lastErr error
	var tempErrSince time.Time // 邮箱服务连续 503/不可用的起始时间
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgs, err := p.mail.ListMessages(ctx, acc, 15)
		if err != nil {
			lastErr = err
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
		} else {
			tempErrSince = time.Time{}
			// 取"最新"一封 OpenAI 验证邮件里的码：重发后新码到达时能覆盖旧码，
			// 避免重发场景下抓回已失效的旧验证码。
			var bestCode string
			var bestAt time.Time
			for _, m := range msgs {
				if m.ReceivedAt.Before(since) || !looksLikeOpenAI(m) {
					continue
				}
				code := ""
				if c := codeRe.FindStringSubmatch(m.Subject); c != nil {
					code = c[1]
				} else if full, gerr := p.mail.GetMessage(ctx, acc, m.ID); gerr == nil {
					if c := codeRe.FindStringSubmatch(full.Subject + " " + full.Text); c != nil {
						code = c[1]
					}
				}
				if code != "" && (bestCode == "" || m.ReceivedAt.After(bestAt)) {
					bestCode, bestAt = code, m.ReceivedAt
				}
			}
			if bestCode != "" {
				return bestCode, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(codePollInterval):
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("超时未收到验证码邮件（读取邮箱最近一次出错: %v）", lastErr)
	}
	return "", fmt.Errorf("超时未收到验证码邮件")
}

func looksLikeOpenAI(m mailfetch.Message) bool {
	s := strings.ToLower(m.From + " " + m.FromName + " " + m.Subject)
	return strings.Contains(s, "openai") || strings.Contains(s, "chatgpt")
}

// ---- inflight / 计数 ----

func (p *Producer) markInflight(email string, mbID uint) {
	p.mu.Lock()
	p.inflight[email] = mbID
	p.mu.Unlock()
}

func (p *Producer) releaseInflight(email string) {
	p.mu.Lock()
	delete(p.inflight, email)
	p.mu.Unlock()
}

func (p *Producer) inflightCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inflight)
}

// mailboxBusy 该邮箱是否已有在跑任务。调用方需持有 claimMu。
func (p *Producer) mailboxBusy(mbID uint) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range p.inflight {
		if id == mbID {
			return true
		}
	}
	return false
}

// counts 返回(已注册成功数, 在跑数)。
func (p *Producer) counts() (int, int) {
	registered := p.registeredCount()
	return registered, p.inflightCount()
}

// producedThisRun 返回本次生产任务已成功产出的账号数（不含库里历史已注册）。
func (p *Producer) producedThisRun() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prog.Registered
}

func (p *Producer) registeredCount() int {
	var n int64
	p.db.Model(&models.Registration{}).Where("status = ?", "registered").Count(&n)
	return int(n)
}

// isRegistered 该地址是否已终结（注册成功 / 已被他人注册 / Terms 拒绝），不再对其发起新注册。
// 注：register_rejected 计入终态以免同一地址被反复重试，但不计入 fissionCount 的裂变名额。
func (p *Producer) isRegistered(email string) bool {
	var n int64
	p.db.Model(&models.Registration{}).Where("email = ? AND status IN ?", email, []string{"registered", "already_registered", "register_rejected"}).Count(&n)
	return n > 0
}

// fissionCount 该邮箱已注册成功或在跑的裂变子号数量（不含母号）。
func (p *Producer) fissionCount(mb models.Mailbox) int {
	var n int64
	q := p.db.Model(&models.Registration{}).
		Where("mailbox_id = ? AND status IN ? AND email <> ?", mb.ID, []string{"registered", "already_registered"}, mb.Email)
	q.Count(&n)
	count := int(n)
	// 加上该邮箱在跑的裂变
	p.mu.Lock()
	for email, id := range p.inflight {
		if id == mb.ID && email != mb.Email {
			count++
		}
	}
	p.mu.Unlock()
	return count
}

// nextFissionEmail 取该邮箱下一个可用的裂变别名。
// 失败(register_failed)的别名会被优先复用重试，而不是每次失败都分配新编号，
// 避免同一邮箱不断“裂变”出 +012/+018/+056 这类跳号的失败记录。
// 失败别名处于冷却期内时整个邮箱暂停开新裂变（cooling=true），避免频繁重试触发风控。
func (p *Producer) nextFissionEmail(base string, cooldown time.Duration) (string, bool) {
	for i := 1; i <= 999; i++ {
		email := emailalias.Address(base, fmt.Sprintf("%03d", i))
		if email == base {
			return "", false
		}
		p.mu.Lock()
		_, busy := p.inflight[email]
		p.mu.Unlock()
		if busy {
			continue
		}
		if p.aliasFinalized(email) {
			continue
		}
		if p.failedRecently(email, cooldown) {
			return "", true // 冷却中：不开新号、也不提前重试，等冷却结束后复用该别名
		}
		return email, false
	}
	return "", false
}

// mailboxUsedRecently 该邮箱最近一次注册（成功或失败）距今不足间隔时间。
// 刚注册完就接着开同一邮箱的下一个裂变，两封验证码邮件容易互相干扰。
func (p *Producer) mailboxUsedRecently(mbID uint, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	var n int64
	p.db.Model(&models.Registration{}).
		Where("mailbox_id = ? AND updated_at > ?", mbID, time.Now().Add(-interval)).
		Count(&n)
	return n > 0
}

// failedRecently 该地址当前处于 register_failed 且距上次失败不足冷却时间。
func (p *Producer) failedRecently(email string, cooldown time.Duration) bool {
	if cooldown <= 0 {
		return false
	}
	var reg models.Registration
	err := p.db.Select("updated_at").
		Where("email = ? AND status = ?", email, "register_failed").
		First(&reg).Error
	if err != nil {
		return false
	}
	return time.Since(reg.UpdatedAt) < cooldown
}

// aliasFinalized 该别名是否已终结（注册成功/已被注册/Terms 拒绝），终结的不再复用；
// register_failed / 遗留的 registering 视为可复用，下一轮重试同一别名。
func (p *Producer) aliasFinalized(email string) bool {
	var n int64
	p.db.Model(&models.Registration{}).
		Where("email = ? AND status IN ?", email, []string{"registered", "already_registered", "register_rejected"}).
		Count(&n)
	return n > 0
}

// ---- DB / 设置 ----

func (p *Producer) loadConfig() Config {
	cfg := Config{
		MaxConcurrency: atoiDefault(p.getSetting("max_concurrency"), defaultMaxConcurrency),
		FissionCount:   atoiDefault(p.getSetting("fission_count"), defaultFissionCount),
		Headless:       p.getSetting("headless") != "0", // 默认无头，仅当设置为 "0" 时才有头
	}
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	if cfg.FissionCount < 0 {
		cfg.FissionCount = defaultFissionCount
	}
	cooldownMin := atoiDefault(p.getSetting("retry_cooldown_min"), defaultRetryCooldownMin)
	if cooldownMin < 0 {
		cooldownMin = 0
	}
	cfg.RetryCooldown = time.Duration(cooldownMin) * time.Minute
	intervalMin := atoiDefault(p.getSetting("mailbox_interval_min"), defaultMailboxIntervalMin)
	if intervalMin < 0 {
		intervalMin = 0
	}
	cfg.MailboxInterval = time.Duration(intervalMin) * time.Minute
	// 代理默认开：未设置(空)视为开，仅显式 "0" 才关闭(直连)。可在设置页开关/编辑。
	if p.getSetting("proxy_enabled") != "0" {
		cfg.Proxies = proxyList(p.getSetting("proxy_list"))
	}
	return cfg
}

// nextProxy 从代理池按轮转取一个；池为空返回空串（直连）。
func (p *Producer) nextProxy(cfg Config) string {
	if len(cfg.Proxies) == 0 {
		return ""
	}
	p.pxMu.Lock()
	proxy := cfg.Proxies[p.pxIdx%len(cfg.Proxies)]
	p.pxIdx++
	p.pxMu.Unlock()
	return proxy
}

func (p *Producer) upsert(reg models.Registration) {
	var existing models.Registration
	if err := p.db.Where("email = ?", reg.Email).First(&existing).Error; err == nil {
		updates := map[string]any{
			"password": reg.Password, "status": reg.Status,
			"is_mother": reg.IsMother, "note": reg.Note, "mailbox_id": reg.MailboxID,
		}
		if reg.AuthData != "" {
			updates["auth_data"] = reg.AuthData
			updates["account_id"] = reg.AccountID
			updates["user_id"] = reg.UserID
			updates["plan_type"] = reg.PlanType
		}
		if reg.Log != "" {
			updates["log"] = reg.Log
		}
		p.db.Model(&existing).Updates(updates)
		return
	}
	p.db.Create(&reg)
}

func (p *Producer) setRegistrationFailed(email, note, log string) {
	p.setRegistrationStatus(email, "register_failed", note, log)
}

func (p *Producer) setRegistrationStatus(email, status, note, log string) {
	upd := map[string]any{"status": status, "note": truncateStr(note, 500)}
	if log != "" { // 为空时保留已实时写入的账号日志，不覆盖
		upd["log"] = log
	}
	p.db.Model(&models.Registration{}).Where("email = ?", email).Updates(upd)
}

func (p *Producer) getSetting(key string) string {
	var s models.Setting
	if err := p.db.Where("key = ?", key).First(&s).Error; err != nil {
		return ""
	}
	return s.Value
}

// ---- 进度 ----

func (p *Producer) logf(format string, a ...any) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, a...)
	p.mu.Lock()
	p.prog.Logs = append(p.prog.Logs, line)
	if len(p.prog.Logs) > maxLogLines {
		p.prog.Logs = p.prog.Logs[len(p.prog.Logs)-maxLogLines:]
	}
	p.prog.UpdatedAt = time.Now()
	p.mu.Unlock()
}

func (p *Producer) incRegistered() {
	p.mu.Lock()
	p.prog.Registered++
	p.recalcLocked()
	p.mu.Unlock()
}

// markFailed 把邮箱标记为失败态，失败数=仍处于失败态的邮箱数。
func (p *Producer) markFailed(email string) {
	p.mu.Lock()
	p.failed[email] = struct{}{}
	p.prog.Failed = len(p.failed)
	p.mu.Unlock()
}

// markSuccess 邮箱最终注册成功，从失败态移除（重试成功不再计入失败）。
func (p *Producer) markSuccess(email string) {
	p.mu.Lock()
	delete(p.failed, email)
	p.prog.Failed = len(p.failed)
	p.mu.Unlock()
}

func (p *Producer) updateProgress() {
	p.mu.Lock()
	p.recalcLocked()
	p.mu.Unlock()
}

// recalcLocked 重新计算 待生产/在跑，调用方需持锁。
func (p *Producer) recalcLocked() {
	p.prog.RunningNum = len(p.inflight)
	pending := p.prog.Target - p.prog.Registered - p.prog.RunningNum
	if pending < 0 {
		pending = 0
	}
	p.prog.Pending = pending
	p.prog.UpdatedAt = time.Now()
}

func (p *Producer) setMessage(msg string) {
	p.mu.Lock()
	p.prog.Message = msg
	p.prog.UpdatedAt = time.Now()
	p.mu.Unlock()
}
