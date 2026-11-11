package gmailproducer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/gmailreg"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/prodcore"
	"chatgpt-register/internal/proxypool"

	"gorm.io/gorm"
)

const maxLogBytes = 64 * 1024

var gmailRegionRe = regexp.MustCompile(`(?i)region[-=]([a-zA-Z]{2})`)

// proxyRegion 从 Gmail 渠道代理配置里提取地区代码（US/GB/...），供浏览器匹配时区/地理位置。
func (p *Producer) proxyRegion() string {
	for _, key := range []string{"proxy_gmail_api_url", "proxy_gmail_list"} {
		if m := gmailRegionRe.FindStringSubmatch(p.Setting(key)); m != nil {
			return strings.ToUpper(m[1])
		}
	}
	return "US"
}

// gmailProfileDir 返回 Gmail 持久化浏览器 Profile 目录（在 rod 浏览器目录旁，与其它渠道隔离）。
func gmailProfileDir() (string, error) {
	base := os.Getenv("APPDATA")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "rod", "gmail-profile")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

type Producer struct {
	*prodcore.Core

	db *gorm.DB

	mu      sync.Mutex
	cancel  map[uint]context.CancelFunc
	topUpCancel context.CancelFunc
	runTarget   int
	runTracked  []uint
}

func New(db *gorm.DB, managers ...*proxypool.Manager) *Producer {
	return &Producer{
		Core:   prodcore.New(db, "gmail", managers...),
		db:     db,
		cancel: map[uint]context.CancelFunc{},
	}
}

func (p *Producer) Start(username, note string) (*models.GmailRegistration, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("用户名不能为空")
	}
	email := username
	if !strings.Contains(email, "@") {
		email = username + "@gmail.com"
	}

	var existing models.GmailRegistration
	if err := p.db.Where("email = ?", email).First(&existing).Error; err == nil {
		if existing.Status == "registering" {
			return nil, fmt.Errorf("该邮箱的 Gmail 注册正在进行中")
		}
		existing.Status = "registering"
		existing.Note = note
		existing.Password = ""
		existing.AuthData = ""
		existing.Shot = nil
		existing.Shipped = false
		if err := p.db.Save(&existing).Error; err != nil {
			return nil, err
		}
		go p.run(existing.ID)
		return &existing, nil
	}

	reg := models.GmailRegistration{Email: email, Status: "registering", Note: note}
	if err := p.db.Create(&reg).Error; err != nil {
		return nil, err
	}
	go p.run(reg.ID)
	return &reg, nil
}

// StartBatch 批量开始注册，count 为目标数量。
func (p *Producer) StartBatch(count int) ([]models.GmailRegistration, error) {
	if count < 1 {
		return nil, fmt.Errorf("数量必须 >= 1")
	}
	started := make([]models.GmailRegistration, 0, count)
	batch := min(count, p.MaxConcurrency())
	for i := 0; i < batch; i++ {
		username := gmailreg.GenRandomUsername()
		email := username + "@gmail.com"
		reg := models.GmailRegistration{Email: email, Status: "registering", Note: "批量生产"}
		if err := p.db.Create(&reg).Error; err != nil {
			continue
		}
		started = append(started, reg)
		go p.run(reg.ID)
	}
	if len(started) == 0 {
		return nil, fmt.Errorf("无法创建注册任务")
	}
	p.beginRun(count, started)
	go p.topUp(count, started)
	return started, nil
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
		p.db.Model(&models.GmailRegistration{}).Where("status IN ?", statuses).Count(&n)
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

	var reg models.GmailRegistration
	if err := p.db.First(&reg, id).Error; err != nil {
		return
	}

	if !p.acquireSlot(ctx, id) {
		p.appendLog(id, "已取消（排队等待空闲注册槽位时被停止）")
		p.db.Model(&models.GmailRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   "已取消",
		})
		return
	}
	defer p.ReleaseSlot()

	p.appendLog(id, "开始 Gmail 注册")
	lease, proxyErr := p.AcquireProxy(ctx)
	proxyCtx, cancelProxy := lease.Context(ctx)
	defer cancelProxy()

	username := strings.TrimSuffix(reg.Email, "@gmail.com")
	// 持久化 Profile:默认开启(让设备积累 Google 信任,降低手机验证墙概率)。
	// 可用设置 gmail_persist_profile=0 关闭。并发必须为 1(Chrome 不能多进程共享 Profile)。
	profileDir := ""
	if strings.TrimSpace(p.Setting("gmail_persist_profile")) != "0" {
		if dir, derr := gmailProfileDir(); derr == nil {
			profileDir = dir
		}
	}
	in := gmailreg.Input{
		Username:   username,
		Proxy:      lease.Proxy,
		Region:     p.proxyRegion(),
		Headless:   p.SettingOn("gmail_headless"),
		ProfileDir: profileDir,
		Log: func(f string, a ...any) {
			p.appendLog(id, fmt.Sprintf(f, a...))
		},
	}

	var res *gmailreg.Result
	err := proxyErr
	if err == nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("浏览器操作 panic: %v", r)
				}
			}()
			res, err = gmailreg.Register(proxyCtx, in)
		}()
	}
	if err != nil {
		p.appendLog(id, "注册失败: "+err.Error())
		p.db.Model(&models.GmailRegistration{}).Where("id = ?", id).Updates(map[string]any{
			"status": "register_failed",
			"note":   prodcore.Truncate(err.Error(), 500),
		})
		return
	}

	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.appendLog(id, "Gmail 注册成功: "+res.Email)
	p.db.Model(&models.GmailRegistration{}).Where("id = ?", id).Updates(map[string]any{
		"status":    "registered",
		"email":     res.Email,
		"password":  res.Password,
		"auth_data": string(authBytes),
	})
}

func (p *Producer) appendLog(id uint, line string) {
	var reg models.GmailRegistration
	if err := p.db.Select("log").First(&reg, id).Error; err != nil {
		return
	}
	p.db.Model(&models.GmailRegistration{}).Where("id = ?", id).
		Update("log", prodcore.AppendLogLine(reg.Log, line, maxLogBytes))
}

func (p *Producer) acquireSlot(ctx context.Context, id uint) bool {
	return p.AcquireSlot(ctx, func() {
		p.appendLog(id, "并发已满，排队等待空闲注册槽位")
	})
}

/* ===== 批量生产补任务 ===== */

func (p *Producer) beginRun(count int, started []models.GmailRegistration) {
	p.mu.Lock()
	p.runTarget = count
	p.runTracked = make([]uint, 0, count)
	for _, reg := range started {
		p.runTracked = append(p.runTracked, reg.ID)
	}
	p.mu.Unlock()
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

func (p *Producer) trackedIDs() []uint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint(nil), p.runTracked...)
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

func (p *Producer) countRegistered(ids []uint) int {
	if len(ids) == 0 {
		return 0
	}
	var n int64
	p.db.Model(&models.GmailRegistration{}).
		Where("id IN ? AND status = ?", ids, "registered").
		Count(&n)
	return int(n)
}

func (p *Producer) runningNum() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.cancel)
}

func (p *Producer) topUp(count int, started []models.GmailRegistration) {
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
		batch := min(remaining, slots)
		var newRegs []models.GmailRegistration
		for i := 0; i < batch; i++ {
			username := gmailreg.GenRandomUsername()
			email := username + "@gmail.com"
			reg := models.GmailRegistration{Email: email, Status: "registering", Note: "批量生产（补单）"}
			if err := p.db.Create(&reg).Error; err != nil {
				continue
			}
			newRegs = append(newRegs, reg)
			go p.run(reg.ID)
		}
		if len(newRegs) == 0 && running == 0 {
			return
		}
		p.mu.Lock()
		for _, reg := range newRegs {
			p.runTracked = append(p.runTracked, reg.ID)
		}
		p.mu.Unlock()
		tracked = p.trackedIDs()
	}
}
