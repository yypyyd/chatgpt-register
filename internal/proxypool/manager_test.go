package proxypool

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chatgpt-register/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "proxy.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	m := New(db)
	t.Cleanup(func() { m.client.CloseIdleConnections() })
	return m
}

func saveAPI(t *testing.T, m *Manager, channel, endpoint string) {
	t.Helper()
	if err := m.Save(channel, Config{Enabled: true, Source: "api", APIURL: endpoint}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentTasksShareOneFetchAndKeepDistinctSessions(t *testing.T) {
	m := testManager(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		for i := 0; i < 24; i++ {
			fmt.Fprintf(w, "http://user-region-US-session-fixed%d-sesstime-10:secret%%2Bpass@proxy.example:8080\r\n", i)
		}
	}))
	defer server.Close()
	saveAPI(t, m, "chatgpt", server.URL+"?sessTime=10&sessType=sticky")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	leases, errs := make(chan Lease, 24), make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := m.Acquire(ctx, "chatgpt", 8*time.Minute)
			if err != nil {
				errs <- err
				return
			}
			leases <- lease
		}()
	}
	wg.Wait()
	close(leases)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]bool{}
	for lease := range leases {
		if seen[lease.Proxy] || !strings.Contains(lease.Proxy, "-session-fixed") || !strings.Contains(lease.Proxy, "-sesstime-10") {
			t.Fatal("session reused or API credentials rewritten")
		}
		seen[lease.Proxy] = true
		ctx, cancel := lease.Context(context.Background())
		deadline, ok := ctx.Deadline()
		cancel()
		if !ok || !deadline.Equal(lease.Deadline) || time.Until(deadline) < 9*time.Minute {
			t.Fatal("rotation deadline not propagated to the registration context")
		}
	}
	if len(seen) != 24 || calls.Load() != 1 {
		t.Fatalf("leases=%d extraction calls=%d", len(seen), calls.Load())
	}
}

func TestChannelsHaveIndependentSwitchesAndPools(t *testing.T) {
	m := testManager(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		fmt.Fprintf(w, "http://session%d:pw@proxy.example:8080\nhttp://extra%d:pw@proxy.example:8080", n, n)
	}))
	defer server.Close()
	for _, channel := range []string{"chatgpt", "grok"} {
		saveAPI(t, m, channel, server.URL)
		lease, err := m.Acquire(context.Background(), channel, time.Minute)
		if err != nil || lease.Proxy == "" {
			t.Fatalf("%s allocation failed: %v", channel, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("channels shared a cache for the same extraction URL")
	}
	if err := m.Save("chatgpt", Config{Source: "api", APIURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Acquire(context.Background(), "chatgpt", time.Minute)
	if err != nil || lease.Proxy != "" {
		t.Fatal("disabled channel did not select direct mode")
	}
	lease, err = m.Acquire(context.Background(), "grok", time.Minute)
	if err != nil || lease.Proxy == "" || calls.Load() != 2 {
		t.Fatal("disabling GPT changed Grok's existing pool")
	}
}

func TestDuplicateAPISessionCannotCrossChannels(t *testing.T) {
	m := testManager(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "http://same-session:pw@proxy.example:8080")
	}))
	defer server.Close()
	for _, channel := range []string{"chatgpt", "grok"} {
		saveAPI(t, m, channel, server.URL)
	}
	if _, err := m.Acquire(context.Background(), "chatgpt", time.Minute); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Acquire(context.Background(), "grok", time.Minute)
	if err == nil || lease.Proxy != "" {
		t.Fatal("duplicate session assigned across channels")
	}
}

func TestExtractionFailuresNeverFallBackToDirectAndDoNotLeakSecrets(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		calls  int32
	}{
		{"empty", 200, "\r\n", 1},
		{"html", 200, "<html>password=upstream-secret</html>", 1},
		{"json-error", 200, `{"error":"upstream-secret"}`, 1},
		{"invalid-port", 200, "http://user:upstream-secret@proxy.example:99999", 1},
		{"unauthorized", 403, "upstream-secret", 1},
		{"temporary", 503, "upstream-secret", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := testManager(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			saveAPI(t, m, "adobe", server.URL+"?key=extraction-secret")
			for i := 0; i < 2; i++ {
				lease, err := m.Acquire(context.Background(), "adobe", time.Minute)
				if err == nil || lease.Proxy != "" {
					t.Fatal("extraction failure returned successful direct mode")
				}
				if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
					t.Fatal("extraction error exposed a response body or API credentials")
				}
			}
			if calls.Load() != test.calls {
				t.Fatalf("retry/backoff calls=%d want=%d", calls.Load(), test.calls)
			}
		})
	}
}

func TestExpiredCacheIsRefetchedAndShortWindowRejectedBeforeFetching(t *testing.T) {
	m := testManager(t)
	var offset atomic.Int64
	started := time.Now()
	m.now = func() time.Time { return started.Add(time.Duration(offset.Load())) }
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		fmt.Fprintf(w, "http://session%d:pw@proxy.example:8080\nhttp://extra%d:pw@proxy.example:8080", n, n)
	}))
	defer server.Close()
	saveAPI(t, m, "chatgpt", server.URL+"?sessTime=10")
	first, err := m.Acquire(context.Background(), "chatgpt", 8*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	offset.Store(int64(31 * time.Second))
	second, err := m.Acquire(context.Background(), "chatgpt", 8*time.Minute)
	if err != nil || calls.Load() != 2 || first.Proxy == second.Proxy {
		t.Fatal("stale cache was not replaced")
	}
	saveAPI(t, m, "chatgpt", server.URL+"?sessTime=5")
	lease, err := m.Acquire(context.Background(), "chatgpt", RegistrationBudget("chatgpt"))
	if err == nil || lease.Proxy != "" || !strings.Contains(err.Error(), "sessTime") || calls.Load() != 2 {
		t.Fatal("short rotation window was accepted or consumed an API extraction")
	}
}

func TestConfigChangeDiscardsInFlightExtraction(t *testing.T) {
	m := testManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, "http://old-session:pw@proxy.example:8080")
	}))
	defer old.Close()
	current := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "http://new-session:pw@proxy.example:8080")
	}))
	defer current.Close()
	saveAPI(t, m, "grok", old.URL)
	result := make(chan Lease, 1)
	errs := make(chan error, 1)
	go func() {
		lease, err := m.Acquire(context.Background(), "grok", time.Minute)
		result <- lease
		errs <- err
	}()
	<-entered
	saveAPI(t, m, "grok", current.URL)
	close(release)
	lease, err := <-result, <-errs
	if err != nil || !strings.Contains(lease.Proxy, "new-session") {
		t.Fatalf("old extraction survived config change: %v", err)
	}
}

func TestWaitingTaskCanBeCancelledWithoutCancellingOtherTask(t *testing.T) {
	m := testManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, "http://session:pw@proxy.example:8080")
	}))
	defer server.Close()
	saveAPI(t, m, "grok", server.URL)
	completed := make(chan error, 1)
	go func() { _, err := m.Acquire(context.Background(), "grok", time.Minute); completed <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { _, err := m.Acquire(ctx, "grok", time.Minute); waiter <- err }()
	cancel()
	select {
	case err := <-waiter:
		if err != context.Canceled {
			t.Fatalf("cancelled waiter returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter remained blocked")
	}
	close(release)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}

func TestMigrationPreservesEffectiveSettingsAndDoesNotResurrectHiddenKeys(t *testing.T) {
	m := testManager(t)
	for key, value := range map[string]string{
		"proxy_enabled": "0", "proxy_list": "http://user-zone-custom-region-US:pw@proxy.bestgo.work:8080",
		"grok_proxy_enabled": "1", "grok_proxy_list": "old hidden value",
	} {
		if err := m.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(m.db); err != nil {
		t.Fatal(err)
	}
	for _, channel := range Channels {
		cfg, err := ReadConfig(m.db, channel)
		if err != nil || cfg.Source != "list" || cfg.List == "" || cfg.Enabled {
			t.Fatalf("bad migration for %s: %v", channel, err)
		}
	}
	if err := m.Save("leonardo", Config{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(m.db); err != nil {
		t.Fatal(err)
	}
	cfg, _ := ReadConfig(m.db, "leonardo")
	if cfg.Enabled || cfg.Source != "api" {
		t.Fatal("migration overwrote a channel's saved configuration")
	}
	lease, err := m.Acquire(context.Background(), "leonardo", time.Minute)
	if err != nil || lease.Proxy != "" {
		t.Fatal("channel bypassed its independent disabled switch")
	}
}

func TestMigrationPreservesExistingChannelConfiguration(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		t.Run(fmt.Sprintf("v1_done_%t", migrated), func(t *testing.T) {
			m := testManager(t)
			values := map[string]string{
				"proxy_enabled": "1", "proxy_list": "http://old-user:old-password@proxy.example:8080",
				"proxy_chatgpt_enabled": "1", "proxy_chatgpt_source": "api", "proxy_chatgpt_api_url": "https://gpt.example/gen?sessTime=10",
			}
			if migrated {
				values[MigrationKey] = "done"
			}
			for key, value := range values {
				if err := m.db.Create(&models.Setting{Key: key, Value: value}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := Migrate(m.db); err != nil {
				t.Fatal(err)
			}
			gpt, _ := ReadConfig(m.db, "chatgpt")
			if !gpt.Enabled || gpt.Source != "api" || gpt.APIURL != values["proxy_chatgpt_api_url"] {
				t.Fatal("migration overwrote the existing GPT channel")
			}
			want := Config{Enabled: true, Source: "api", APIURL: "https://leonardo.example/gen?count=1&sessTime=10"}
			if err := m.Save("leonardo", want); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(m.db); err != nil {
				t.Fatal(err)
			}
			got, err := ReadConfig(m.db, "leonardo")
			if err != nil || got != want {
				t.Fatalf("restart overwrote the saved channel configuration: %+v %v", got, err)
			}
		})
	}
}

func TestAPIParserSupportsProviderFormatsAndPreservesCredentials(t *testing.T) {
	for _, body := range []string{
		"\ufeffproxy.example:8080:user+name:p@ss\r\nproxy.example:8080:user+name:p@ss\r\n",
		"http://user+name:p%40ss@proxy.example:8080\\r\\nhttp://user+name:p%40ss@proxy.example:8080",
	} {
		proxies, err := parseList(body)
		if err != nil || len(proxies) != 1 {
			t.Fatalf("format or deduplication failed: %v", err)
		}
		u, _ := url.Parse(proxies[0])
		password, _ := u.User.Password()
		if u.User.Username() != "user+name" || password != "p@ss" {
			t.Fatal("proxy credentials changed during normalization")
		}
	}
}

func TestMissingConfigurationAndDatabaseErrorsFailSafely(t *testing.T) {
	m := testManager(t)
	if err := m.Save("grok", Config{Enabled: true, Source: "api"}); err == nil {
		t.Fatal("enabled channel accepted an empty extraction API")
	}
	if _, err := m.Acquire(context.Background(), "unknown", time.Minute); err == nil {
		t.Fatal("unknown channel accepted")
	}
	if err := m.Save("grok", Config{Source: "api", APIURL: "broken-api", List: "broken legacy list"}); err != nil {
		t.Fatal("invalid draft prevented disabling the independent switch")
	}
	lease, err := m.Acquire(context.Background(), "grok", time.Minute)
	if err != nil || lease.Proxy != "" {
		t.Fatal("disabled channel still tried to validate/use a broken source")
	}
	if err := m.db.Migrator().DropTable(&models.Setting{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(context.Background(), "grok", time.Minute); err == nil {
		t.Fatal("database failure silently selected direct mode")
	}
}

func TestSlowExtractionDoesNotBlockAnotherChannel(t *testing.T) {
	m := testManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, "http://slow-session:pw@proxy.example:8080")
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "http://fast-session:pw@proxy.example:8080")
	}))
	defer fast.Close()
	saveAPI(t, m, "chatgpt", slow.URL)
	saveAPI(t, m, "grok", fast.URL)
	done := make(chan error, 1)
	go func() { _, err := m.Acquire(context.Background(), "chatgpt", time.Minute); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lease, err := m.Acquire(ctx, "grok", time.Minute)
	close(release)
	if err != nil || !strings.Contains(lease.Proxy, "fast-session") {
		t.Errorf("slow GPT extraction blocked Grok: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTemporaryExtractionErrorRecoversWithinRetryLimit(t *testing.T) {
	m := testManager(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "http://recovered-session:pw@proxy.example:8080")
	}))
	defer server.Close()
	saveAPI(t, m, "grok", server.URL)
	lease, err := m.Acquire(context.Background(), "grok", time.Minute)
	if err != nil || lease.Proxy == "" || calls.Load() != 2 {
		t.Fatalf("temporary extraction error failed to recover: %v, calls=%d", err, calls.Load())
	}
}

func TestSavingOneChannelLeavesRunningLeaseAndOtherCacheIntact(t *testing.T) {
	m := testManager(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "http://%s-session:pw@proxy.example:8080\nhttp://%s-extra:pw@proxy.example:8080", r.URL.Path[1:], r.URL.Path[1:])
	}))
	defer server.Close()
	saveAPI(t, m, "chatgpt", server.URL+"/gpt?sessTime=10")
	saveAPI(t, m, "grok", server.URL+"/grok?sessTime=10")
	lease, err := m.Acquire(context.Background(), "chatgpt", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(context.Background(), "grok", time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := lease.Context(context.Background())
	defer cancel()
	if err := m.Save("chatgpt", Config{Source: "api"}); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || !strings.Contains(lease.Proxy, "gpt-session") {
		t.Fatal("saving channel configuration changed a running lease")
	}
	cfg, _ := ReadConfig(m.db, "grok")
	if m.Status("grok", cfg).Available != 1 {
		t.Fatal("saving GPT cleared Grok's available proxy cache")
	}
}
