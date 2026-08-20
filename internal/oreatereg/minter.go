package oreatereg

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	// minterSessionMints 一个常驻会话最多铸多少个 jt 就换新会话：同一个 OUID
	// 铸太多次容易被站点风控盯上。
	minterSessionMints = 30

	// minterSessionTTL 常驻会话的最长存活时间。
	minterSessionTTL = 20 * time.Minute
)

// Token 是一次铸造结果。jt 是一次性的，且与铸造它的 OUID/UA 绑定：调用方发生成请求时
// 必须带上同一套 OUID/UA。出口 IP 不参与校验，调用方可以从自己的出口发请求。
type Token struct {
	JT        string `json:"jt"`
	OUID      string `json:"ouid"`
	UserAgent string `json:"user_agent"`
	BID       string `json:"bid"`
}

// Minter 按代理出口维护常驻浏览器会话，按需铸造反爬 token 供 2api 调用。
// 一个出口一个会话：铸造要串行（同一个页面），页面留着复用可以省掉每次几十秒的启动。
type Minter struct {
	mu    sync.Mutex
	slots map[string]*mintSlot
}

type mintSlot struct {
	mu   sync.Mutex
	sess *session
	// ctx/cancel 是会话自己的生命周期，不能用单次请求的 ctx：
	// 页面绑定的 ctx 一取消，整个会话就不能再用了。
	ctx     context.Context
	cancel  context.CancelFunc
	minted  int
	created time.Time
}

func NewMinter() *Minter {
	return &Minter{slots: map[string]*mintSlot{}}
}

// Mint 用 proxy 对应的常驻会话铸一个 jt；会话不存在、已到期或铸造失败时重开一次。
func (m *Minter) Mint(proxy string, in Input) (*Token, error) {
	slot := m.slot(proxy)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	in.Proxy = proxy
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if slot.stale() {
			slot.reset()
		}
		if slot.sess == nil {
			slot.ctx, slot.cancel = context.WithCancel(context.Background())
			sess, err := openSession(slot.ctx, in)
			if err != nil {
				lastErr = err
				slot.reset()
				continue
			}
			slot.sess, slot.created, slot.minted = sess, time.Now(), 0
		}
		jt, err := slot.sess.mint(slot.ctx, in)
		if err != nil {
			lastErr = err
			slot.reset()
			continue
		}
		slot.minted++
		return &Token{
			JT:        jt,
			OUID:      slot.sess.OUID,
			UserAgent: slot.sess.UA,
			BID:       slot.sess.BID,
		}, nil
	}
	return nil, fmt.Errorf("铸造反爬 token 失败: %w", lastErr)
}

// Close 关掉所有常驻会话。
func (m *Minter) Close() {
	m.mu.Lock()
	slots := make([]*mintSlot, 0, len(m.slots))
	for _, slot := range m.slots {
		slots = append(slots, slot)
	}
	m.slots = map[string]*mintSlot{}
	m.mu.Unlock()
	for _, slot := range slots {
		slot.mu.Lock()
		slot.reset()
		slot.mu.Unlock()
	}
}

func (m *Minter) slot(proxy string) *mintSlot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slot := m.slots[proxy]; slot != nil {
		return slot
	}
	slot := &mintSlot{}
	m.slots[proxy] = slot
	return slot
}

func (s *mintSlot) stale() bool {
	return s.sess != nil &&
		(s.minted >= minterSessionMints || time.Since(s.created) > minterSessionTTL)
}

func (s *mintSlot) reset() {
	if s.sess != nil {
		s.sess.close()
		s.sess = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.ctx = nil
	s.minted = 0
}
