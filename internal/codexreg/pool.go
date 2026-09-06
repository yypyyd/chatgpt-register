package codexreg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// PoolOptions 浏览器池参数。
type PoolOptions struct {
	Headless   bool
	BrowserBin string
	// ContextsPerHost 一个 Chrome 进程最多同时承载多少个账号上下文。上下文之间只共享进程，
	// 不共享 cookie / 缓存 / 出口 / 窗口；再多会互相抢渲染进程与 CPU，反而拖慢每个号。
	ContextsPerHost int
	// HostMaxAge 一个 Chrome 进程最多活多久：长时间跑的进程内存会缓慢上涨，到期后不再分配新上下文，
	// 等已有账号跑完自然退出。
	HostMaxAge time.Duration
	// HostMaxContexts 一个进程累计承载过多少个上下文就退役（同上，防止内存慢涨）。
	HostMaxContexts int
	Log             func(format string, a ...any)
}

// Pool 共享 Chrome 进程的账号上下文池。
//
// 每个账号拿到的是 Target.createBrowserContext 建出来的独立上下文：自己的 cookie jar、
// 缓存、localStorage、代理出口（proxyServer 挂在上下文上）、窗口尺寸、屏幕规格、语言与时区。
// 进程级的东西（Chrome 版本、WebGL、字体、CPU 核数）本来就来自同一台机器，共享进程不会
// 增加任何账号之间的关联；省下的是每号一个进程的 150~300MB 内存与 1~3 秒启动。
type Pool struct {
	opt PoolOptions
	mu  sync.Mutex
	// hosts 按创建顺序排列，分配时优先填满最旧的可用进程。
	hosts  []*poolHost
	closed bool
}

type poolHost struct {
	h        *host
	created  time.Time
	active   int
	served   int
	retiring bool
}

// NewPool 建池；进程按需惰性启动，Close 前一直复用。
func NewPool(opt PoolOptions) *Pool {
	if opt.ContextsPerHost < 1 {
		opt.ContextsPerHost = 4
	}
	if opt.HostMaxAge <= 0 {
		opt.HostMaxAge = 45 * time.Minute
	}
	if opt.HostMaxContexts <= 0 {
		opt.HostMaxContexts = 40
	}
	if opt.Log == nil {
		opt.Log = func(string, ...any) {}
	}
	return &Pool{opt: opt}
}

// ContextOptions 单个账号上下文的参数。
type ContextOptions struct {
	Proxy     string
	Locale    string
	Languages string
	UserAgent string
	Screen    *ScreenProfile
	Timezone  string
	Log       func(format string, a ...any)
}

// ErrPoolClosed 池已关闭。
var ErrPoolClosed = errors.New("浏览器池已关闭")

// Acquire 为一个账号分配独立上下文。ctx 只影响该账号 CDP 调用的取消，不影响进程生命周期。
func (p *Pool) Acquire(ctx context.Context, opt ContextOptions) (*Session, error) {
	logf := opt.Log
	if logf == nil {
		logf = p.opt.Log
	}
	locale, languages := normalizeLocale(opt.Locale, opt.Languages)

	server, bridge, authUser, authPass, err := prepareProxy(opt.Proxy, logf)
	if err != nil {
		return nil, err
	}
	if authUser != "" || authPass != "" {
		// 上下文级代理没有对应的认证钩子（HandleAuth 是进程级的，会串到别的账号），
		// socks5 带账号密码在池模式下不支持。
		if bridge != nil {
			bridge.Close()
		}
		return nil, fmt.Errorf("池模式仅支持无认证或 http(s) 认证代理，不支持带账号密码的 %s", server)
	}

	ph, err := p.pickHost(locale)
	if err != nil {
		if bridge != nil {
			bridge.Close()
		}
		return nil, err
	}
	res, err := (proto.TargetCreateBrowserContext{
		DisposeOnDetach: true,
		ProxyServer:     server,
	}).Call(ph.h.browser)
	if err != nil {
		p.mu.Lock()
		ph.active--
		p.mu.Unlock()
		if bridge != nil {
			bridge.Close()
		}
		return nil, fmt.Errorf("创建浏览器上下文失败: %w", err)
	}
	// rod.Browser 是值拷贝 + 上下文 ID：同一 CDP 连接，只是 Page() 会在该上下文里开标签页。
	b := *ph.h.browser
	b = *b.Context(ctx)
	b.BrowserContextID = res.BrowserContextID

	userAgent := ph.h.ua
	if strings.TrimSpace(opt.UserAgent) != "" {
		userAgent = cleanUserAgent(opt.UserAgent)
	}
	screen := pickScreenProfile()
	if opt.Screen != nil && opt.Screen.valid() {
		screen = *opt.Screen
	}
	sess := &Session{
		Browser:    &b,
		UserAgent:  userAgent,
		Platform:   platformForUA(userAgent),
		Screen:     screen,
		BrowserBin: ph.h.bin,
		host:       ph.h,
		contextID:  res.BrowserContextID,
		pool:       p,
		locale:     locale,
		languages:  languages,
		timezone:   strings.TrimSpace(opt.Timezone),
		bridge:     bridge,
		log:        logf,
	}
	logf("🖥 已分配浏览器上下文 | 屏幕 %s | 进程 %s", sess.Screen, ph.h.describeBin())
	return sess, nil
}

// pickHost 选一个还有名额、未退役的进程；没有就新起一个。返回时已把 active 计数占好。
func (p *Pool) pickHost(locale string) (*poolHost, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}
	for _, ph := range p.hosts {
		if ph.retiring || ph.active >= p.opt.ContextsPerHost {
			continue
		}
		if time.Since(ph.created) > p.opt.HostMaxAge || ph.served >= p.opt.HostMaxContexts {
			ph.retiring = true
			continue
		}
		ph.active++
		ph.served++
		p.mu.Unlock()
		return ph, nil
	}
	// 先占位再启动：启动要几秒，期间别让别的任务也去起新进程。
	ph := &poolHost{created: time.Now(), active: 1, served: 1}
	p.hosts = append(p.hosts, ph)
	p.mu.Unlock()

	// 进程用后台 ctx：单个任务的 ctx 取消不能把整个进程带走。
	h, err := launchHost(context.Background(), p.opt.Headless, p.opt.BrowserBin, locale, "", p.opt.Log)
	p.mu.Lock()
	if err != nil {
		p.removeHostLocked(ph)
		p.mu.Unlock()
		return nil, err
	}
	ph.h = h
	if p.closed {
		p.removeHostLocked(ph)
		p.mu.Unlock()
		h.close()
		return nil, ErrPoolClosed
	}
	p.mu.Unlock()
	return ph, nil
}

// release 销毁账号上下文并归还名额；退役进程在最后一个上下文释放后关闭。
func (p *Pool) release(s *Session) {
	if s.contextID != "" {
		_ = rod.Try(func() {
			_ = (proto.TargetDisposeBrowserContext{BrowserContextID: s.contextID}).Call(s.host.browser.Context(context.Background()))
		})
	}
	p.mu.Lock()
	var ph *poolHost
	for _, cand := range p.hosts {
		if cand.h == s.host {
			ph = cand
			break
		}
	}
	if ph == nil {
		p.mu.Unlock()
		return
	}
	ph.active--
	closeNow := ph.retiring && ph.active <= 0
	if closeNow {
		p.removeHostLocked(ph)
	}
	p.mu.Unlock()
	if closeNow {
		p.opt.Log("♻ 浏览器进程已到期，关闭并在需要时重新启动")
		ph.h.close()
	}
}

func (p *Pool) removeHostLocked(target *poolHost) {
	for i, ph := range p.hosts {
		if ph == target {
			p.hosts = append(p.hosts[:i], p.hosts[i+1:]...)
			return
		}
	}
}

// Stats 返回 (进程数, 活跃上下文数)。
func (p *Pool) Stats() (hosts, active int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ph := range p.hosts {
		if ph.h != nil {
			hosts++
		}
		active += ph.active
	}
	return
}

// Close 关掉所有进程。正在跑的账号会因 CDP 连接断开而失败，调用方应在所有任务结束后再调用。
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	hosts := p.hosts
	p.hosts = nil
	p.mu.Unlock()
	for _, ph := range hosts {
		if ph.h != nil {
			ph.h.close()
		}
	}
}
