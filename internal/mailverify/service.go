// Package mailverify runs durable, bounded-concurrency mailbox authentication.
package mailverify

import (
	"context"
	"log"
	"sync"
	"time"

	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"

	"gorm.io/gorm"
)

const DefaultConcurrency = 10

// Verifier is implemented by mailfetch.Client and kept small for deterministic tests.
type Verifier interface {
	Verify(context.Context, mailfetch.Account) error
}

// Service treats unverified rows as durable queued jobs and verifying rows as claimed jobs.
// In-flight IDs are tracked so a re-authentication request cannot run the same mailbox twice.
type Service struct {
	db          *gorm.DB
	verifier    Verifier
	concurrency int

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	wg     sync.WaitGroup

	claimMu  sync.Mutex
	activeMu sync.Mutex
	active   map[uint]struct{}
	stopOnce sync.Once
}

func New(db *gorm.DB, verifier Verifier, concurrency int) *Service {
	if concurrency < 1 {
		concurrency = DefaultConcurrency
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		db: db, verifier: verifier, concurrency: concurrency,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), active: map[uint]struct{}{},
	}
}

// Start recovers interrupted jobs and launches the worker pool.
func (s *Service) Start() error {
	if err := s.db.Model(&models.Mailbox{}).
		Where("status = ?", "verifying").Update("status", "unverified").Error; err != nil {
		return err
	}
	for i := 0; i < s.concurrency; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	s.Wake()
	return nil
}

func (s *Service) Stop() {
	s.stopOnce.Do(s.cancel)
	s.wg.Wait()
}

// Wake notifies idle workers after new unverified rows have been inserted.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Reauthenticate marks the requested IDs for a fresh authentication attempt.
func (s *Service) Reauthenticate(ids []uint) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	r := s.db.Model(&models.Mailbox{}).Where("id IN ?", ids).Update("status", "unverified")
	if r.Error == nil {
		s.Wake()
	}
	return r.RowsAffected, r.Error
}

// ReauthenticateFailed queues only mailboxes whose previous authentication failed.
func (s *Service) ReauthenticateFailed() (int64, error) {
	r := s.db.Model(&models.Mailbox{}).Where("status = ?", "verify_failed").Update("status", "unverified")
	if r.Error == nil {
		s.Wake()
	}
	return r.RowsAffected, r.Error
}

// ReauthenticateAll queues every mailbox, including previously verified and failed rows.
func (s *Service) ReauthenticateAll() (int64, error) {
	r := s.db.Model(&models.Mailbox{}).Where("1 = 1").Update("status", "unverified")
	if r.Error == nil {
		s.Wake()
	}
	return r.RowsAffected, r.Error
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		mailbox, ok, err := s.claim()
		if err != nil {
			log.Printf("mailbox verifier claim: %v", err)
			if !s.wait(500 * time.Millisecond) {
				return
			}
			continue
		}
		if !ok {
			if !s.wait(5 * time.Second) {
				return
			}
			continue
		}

		status := "verified"
		if err := s.verifier.Verify(s.ctx, mailfetch.Account{
			Email: mailbox.Email, ClientID: mailbox.ClientID, RefreshToken: mailbox.RefreshToken,
		}); err != nil {
			status = "verify_failed"
		}
		// A re-authentication request may have reset this row to unverified while the
		// network call was running. In that case, preserve the newly queued state.
		if err := s.db.Model(&models.Mailbox{}).
			Where("id = ? AND status = ?", mailbox.ID, "verifying").
			Update("status", status).Error; err != nil {
			log.Printf("mailbox verifier finish id=%d: %v", mailbox.ID, err)
		}
		s.release(mailbox.ID)
	}
}

func (s *Service) claim() (models.Mailbox, bool, error) {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()

	var candidates []models.Mailbox
	limit := s.concurrency*2 + 1
	if err := s.db.Where("status = ?", "unverified").Order("id desc").Limit(limit).Find(&candidates).Error; err != nil {
		return models.Mailbox{}, false, err
	}
	for _, mailbox := range candidates {
		if !s.reserve(mailbox.ID) {
			continue
		}
		r := s.db.Model(&models.Mailbox{}).
			Where("id = ? AND status = ?", mailbox.ID, "unverified").
			Update("status", "verifying")
		if r.Error != nil {
			s.release(mailbox.ID)
			return models.Mailbox{}, false, r.Error
		}
		if r.RowsAffected == 1 {
			return mailbox, true, nil
		}
		s.release(mailbox.ID)
	}
	return models.Mailbox{}, false, nil
}

func (s *Service) reserve(id uint) bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if _, exists := s.active[id]; exists {
		return false
	}
	s.active[id] = struct{}{}
	return true
}

func (s *Service) release(id uint) {
	s.activeMu.Lock()
	delete(s.active, id)
	s.activeMu.Unlock()
	s.Wake()
}

func (s *Service) wait(delay time.Duration) bool {
	select {
	case <-s.ctx.Done():
		return false
	case <-s.wake:
		return true
	case <-time.After(delay):
		return true
	}
}
