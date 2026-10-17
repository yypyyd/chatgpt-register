package proxypool

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"chatgpt-register/internal/models"
	"chatgpt-register/internal/proxyutil"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	cacheLifetime  = 30 * time.Second
	rotationMargin = 30 * time.Second
	failureBackoff = 5 * time.Second
)

type Lease struct {
	Proxy    string
	Deadline time.Time
}

// Context stops network work before the configured upstream rotation boundary.
func (l Lease) Context(ctx context.Context) (context.Context, context.CancelFunc) {
	if !l.Deadline.IsZero() {
		return context.WithDeadline(ctx, l.Deadline)
	}
	return context.WithCancel(ctx)
}

type entry struct {
	proxy    string
	cachedTo time.Time
	deadline time.Time
}

type pool struct {
	fingerprint string
	entries     []entry
	cursor      int
	fetching    chan struct{}
	lastFetch   *time.Time
	lastError   string
	retryAfter  time.Time
}

type Status struct {
	Available int        `json:"available"`
	Fetching  bool       `json:"fetching"`
	LastFetch *time.Time `json:"last_fetch"`
	LastError string     `json:"last_error"`
}

type Manager struct {
	db     *gorm.DB
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
	pools  map[string]*pool
	used   map[string]time.Time
}

func New(db *gorm.DB) *Manager {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Manager{
		db: db, now: time.Now, pools: map[string]*pool{}, used: map[string]time.Time{},
		client: &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("提取 API 重定向过多")
			}
			_, err := parseAPIURL(req.URL.String())
			return err
		}},
	}
}

// Select lets existing constructors keep their two-argument form in tests.
func Select(db *gorm.DB, managers ...*Manager) *Manager {
	if len(managers) > 0 && managers[0] != nil {
		return managers[0]
	}
	return New(db)
}

func (m *Manager) Save(channel string, cfg Config) error {
	if !ValidChannel(channel) {
		return fmt.Errorf("未知代理渠道")
	}
	cfg = cfg.normalized()
	if err := cfg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.db.Transaction(func(tx *gorm.DB) error {
		for key, value := range configValues(channel, cfg) {
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("保存渠道代理配置失败")
	}
	p := m.poolLocked(channel, cfg.fingerprint())
	p.entries, p.lastError, p.retryAfter = nil, "", time.Time{}
	return nil
}

func (m *Manager) poolLocked(channel, fingerprint string) *pool {
	p := m.pools[channel]
	if p == nil {
		p = &pool{}
		m.pools[channel] = p
	}
	if p.fingerprint != fingerprint {
		p.fingerprint = fingerprint
		p.entries, p.cursor, p.lastError, p.retryAfter = nil, 0, "", time.Time{}
	}
	return p
}

func usable(e entry, now time.Time, minValidity time.Duration) bool {
	return now.Before(e.cachedTo) && (e.deadline.IsZero() || now.Add(minValidity).Before(e.deadline))
}

func (m *Manager) Status(channel string, cfg Config) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pools[channel]
	if p == nil || p.fingerprint != cfg.normalized().fingerprint() {
		return Status{}
	}
	status := Status{Fetching: p.fetching != nil, LastFetch: p.lastFetch, LastError: p.lastError}
	for _, e := range p.entries {
		if usable(e, m.now(), RegistrationBudget(channel)) && !m.now().Before(m.used[e.proxy]) {
			status.Available++
		}
	}
	return status
}

func windowError(channel string, duration, budget time.Duration) error {
	return fmt.Errorf("%s 代理轮换时间 %d 分钟不足以覆盖本次流程，请把 API 的 sessTime 设为至少 %d 分钟", Label(channel), int(duration/time.Minute), int((budget+rotationMargin)/time.Minute)+1)
}

// Acquire returns empty Proxy only when this channel is explicitly disabled.
// API credentials/session parameters are never rewritten. Each returned API
// entry is consumed once, with a shared reservation against cross-channel reuse.
func (m *Manager) Acquire(ctx context.Context, channel string, minValidity time.Duration) (Lease, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Lease{}, err
		}
		// Serialize the config read with saves and cache assignment, so a task
		// cannot allocate a stale cached entry after a save has completed.
		m.mu.Lock()
		cfg, err := ReadConfig(m.db.WithContext(ctx), channel)
		if err != nil {
			m.mu.Unlock()
			return Lease{}, err
		}
		cfg = cfg.normalized()
		if !cfg.Enabled {
			m.mu.Unlock()
			return Lease{}, nil
		}
		if err := cfg.Validate(); err != nil {
			m.mu.Unlock()
			return Lease{}, err
		}
		duration, _ := cfg.sessionDuration()
		if duration > 0 && duration <= minValidity+rotationMargin {
			m.mu.Unlock()
			return Lease{}, windowError(channel, duration, minValidity)
		}
		fingerprint := cfg.fingerprint()
		p := m.poolLocked(channel, fingerprint)
		now := m.now()
		if cfg.Source == "list" {
			proxies, err := parseList(cfg.List)
			if err != nil {
				m.mu.Unlock()
				return Lease{}, err
			}
			proxy := proxies[p.cursor%len(proxies)]
			p.cursor++
			m.mu.Unlock()
			return Lease{Proxy: proxyutil.WithBestGoTaskSession(proxy)}, nil
		}
		for raw, until := range m.used {
			if !now.Before(until) {
				delete(m.used, raw)
			}
		}
		for len(p.entries) > 0 {
			e := p.entries[0]
			p.entries = p.entries[1:]
			if !usable(e, now, minValidity) || now.Before(m.used[e.proxy]) {
				continue
			}
			until := e.deadline
			if until.IsZero() {
				until = now.Add(max(minValidity+rotationMargin, 10*time.Minute))
			}
			m.used[e.proxy] = until
			m.mu.Unlock()
			return Lease{Proxy: e.proxy, Deadline: e.deadline}, nil
		}
		if p.fetching != nil {
			wait := p.fetching
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return Lease{}, ctx.Err()
			case <-wait:
				continue
			}
		}
		if p.lastError != "" && now.Before(p.retryAfter) {
			err := fmt.Errorf("%s", p.lastError)
			m.mu.Unlock()
			return Lease{}, err
		}
		finished := make(chan struct{})
		p.fetching = finished
		m.mu.Unlock()

		started := m.now()
		proxies, fetchErr := m.Fetch(ctx, cfg)
		m.mu.Lock()
		latest, readErr := ReadConfig(m.db.WithContext(ctx), channel)
		p.fetching = nil
		close(finished)
		// A save while HTTP was in flight discards the old response entirely.
		if readErr == nil && (latest.normalized().fingerprint() != fingerprint || p.fingerprint != fingerprint) {
			m.mu.Unlock()
			continue
		}
		if fetchErr == nil && readErr != nil {
			fetchErr = readErr
		}
		if fetchErr == nil {
			p.lastFetch = &started
			deadline := time.Time{}
			if duration > 0 {
				deadline = started.Add(duration - rotationMargin)
			}
			for _, proxy := range proxies {
				e := entry{proxy: proxy, cachedTo: started.Add(cacheLifetime), deadline: deadline}
				if usable(e, m.now(), minValidity) && !m.now().Before(m.used[proxy]) {
					p.entries = append(p.entries, e)
				}
			}
			if len(p.entries) == 0 {
				fetchErr = fmt.Errorf("API 返回的代理会话已分配或剩余时间不足，请提取新的会话")
			}
		}
		if fetchErr != nil {
			if ctx.Err() == nil {
				p.lastError, p.retryAfter = fetchErr.Error(), m.now().Add(failureBackoff)
			}
			m.mu.Unlock()
			return Lease{}, fetchErr
		}
		p.lastError, p.retryAfter = "", time.Time{}
		m.mu.Unlock()
	}
}
