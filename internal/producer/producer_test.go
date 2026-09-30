package producer

import (
	"context"
	"testing"
	"time"

	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestGPTConcurrencyIsCappedAndStaggerSettingIsApplied(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"max_concurrency": "99", "launch_stagger_sec": "17",
	} {
		if err := db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	p := New(db, nil)
	cfg := p.loadConfig()
	if cfg.MaxConcurrency != 99 {
		t.Fatalf("GPT concurrency setting ignored: %d", cfg.MaxConcurrency)
	}
	if cfg.LaunchStagger != 17*time.Second {
		t.Fatalf("stagger setting ignored: %s", cfg.LaunchStagger)
	}
}

func TestWaitLaunchSlotHonorsCancellation(t *testing.T) {
	p := &Producer{lastLaunch: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if p.waitLaunchSlot(ctx, time.Minute) {
		t.Fatal("cancelled launch slot was accepted")
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled wait did not return promptly")
	}
}
