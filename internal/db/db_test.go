package db

import (
	"path/filepath"
	"testing"

	"chatgpt-register/internal/models"
)

func TestInitKeepsActiveInventoryAndPreservesRetiredData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")
	database, err := Init(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"createvision_registrations", "lumina_registrations", "dola_registrations", "facebook_accounts", "dola_google_mailboxes"} {
		if database.Migrator().HasTable(table) {
			t.Fatalf("retired inventory was created: %s", table)
		}
	}
	// Removing a feature must not erase data from databases already in use.
	if err := database.Exec(`CREATE TABLE createvision_registrations (id INTEGER PRIMARY KEY, auth_data TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Exec(`INSERT INTO createvision_registrations (id, auth_data) VALUES (1, 'stored-retired-session')`).Error; err != nil {
		t.Fatal(err)
	}
	regs := []models.GrokRegistration{
		{Email: "running@example.com", Status: "registering"},
		{Email: "waiting@example.com", Status: "waiting_code"},
		{Email: "saved@example.com", Status: "registered", AuthData: `{"access_token":"stored-token"}`, Shipped: true},
	}
	if err := database.Create(&regs).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&models.Registration{Email: regs[2].Email, Status: "registered"}).Error; err != nil {
		t.Fatal("Grok inventory conflicts with GPT inventory:", err)
	}
	sqlDB, _ := database.DB()
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Init(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ = database.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	var retired string
	if err := database.Raw(`SELECT auth_data FROM createvision_registrations WHERE id=1`).Scan(&retired).Error; err != nil || retired != "stored-retired-session" {
		t.Fatal("startup removed historical inventory:", err)
	}
	var got []models.GrokRegistration
	if err := database.Order("id asc").Find(&got).Error; err != nil || len(got) != 3 {
		t.Fatalf("reload inventory: %v, records=%d", err, len(got))
	}
	for _, reg := range got[:2] {
		if reg.Status != "register_failed" || reg.Note != "程序重启中断，可重新注册" {
			t.Fatalf("orphan was not reclaimed: %+v", reg)
		}
	}
	if got[2].Status != "registered" || got[2].AuthData != regs[2].AuthData || !got[2].Shipped {
		t.Fatal("restart changed a successful account")
	}
}

func TestHasStoredLongLivedCredential(t *testing.T) {
	for _, raw := range []string{
		`{"refresh_token":"rt"}`,
		`{"cookies":[{"name":"session","value":"secret"}]}`,
	} {
		if !hasStoredLongLivedCredential(raw) {
			t.Fatalf("stored credential not detected: %s", raw)
		}
	}
	for _, raw := range []string{`{}`, `{"refresh_token":"","cookies":[]}`, `not-json`} {
		if hasStoredLongLivedCredential(raw) {
			t.Fatalf("missing credential reported as present: %s", raw)
		}
	}
}
