package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestApiKeyMigrationFromLegacyField(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	// Seed a config file in the legacy shape (no apiKeys list, single ApiKey field).
	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"apiKey":        "legacy-secret",
		"requireApiKey": true,
		"accounts":      []interface{}{},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	keys := ListApiKeys()
	if len(keys) != 1 {
		t.Fatalf("expected one migrated key, got %d", len(keys))
	}
	migrated := keys[0]
	if migrated.Key != "legacy-secret" {
		t.Fatalf("expected migrated key value, got %q", migrated.Key)
	}
	if !migrated.Migrated {
		t.Fatalf("expected migrated flag to be true")
	}
	if !migrated.Enabled {
		t.Fatalf("expected migrated key to be enabled")
	}
	if migrated.ID == "" {
		t.Fatalf("expected migrated key to have an ID")
	}

	// Reload and confirm migration was persisted (no second migration entry appears).
	if err := Init(cfgFile); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if got := len(ListApiKeys()); got != 1 {
		t.Fatalf("expected migration to be idempotent, got %d entries", got)
	}
}

// Public deployments (RequireApiKey=false) must not silently start enforcing
// auth after upgrade. The migrated legacy entry is created disabled so the
// service stays open until an operator explicitly toggles auth on.
func TestApiKeyMigrationPublicDeploymentStaysDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"apiKey":        "legacy-secret",
		"requireApiKey": false,
		"accounts":      []interface{}{},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	keys := ListApiKeys()
	if len(keys) != 1 {
		t.Fatalf("expected one migrated key, got %d", len(keys))
	}
	if keys[0].Enabled {
		t.Fatalf("expected migrated key to be disabled when legacy deployment was public")
	}
	if !keys[0].Migrated {
		t.Fatalf("expected migrated flag to remain set")
	}
}

func TestApiKeyCRUD(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	created, err := AddApiKey(ApiKeyEntry{Name: "alpha", Key: "sk-alpha", Enabled: true, TokenLimit: 1000})
	if err != nil {
		t.Fatalf("add alpha: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("expected ID to be assigned")
	}
	if created.CreatedAt == 0 {
		t.Fatalf("expected CreatedAt to be set")
	}

	if _, err := AddApiKey(ApiKeyEntry{Name: "dup", Key: "sk-alpha", Enabled: true}); err == nil {
		t.Fatalf("expected duplicate add to fail")
	}

	if _, err := AddApiKey(ApiKeyEntry{Name: "empty", Key: "", Enabled: true}); err == nil {
		t.Fatalf("expected empty key add to fail")
	}

	if err := UpdateApiKey(created.ID, ApiKeyEntry{
		Name:        "alpha-renamed",
		Enabled:     false,
		TokenLimit:  2000,
		CreditLimit: 5.5,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil {
		t.Fatalf("expected entry to exist after update")
	}
	if got.Name != "alpha-renamed" {
		t.Fatalf("expected name to be updated, got %q", got.Name)
	}
	if got.Enabled {
		t.Fatalf("expected enabled to be flipped off")
	}
	if got.TokenLimit != 2000 || got.CreditLimit != 5.5 {
		t.Fatalf("expected limits to be updated, got token=%d credit=%v", got.TokenLimit, got.CreditLimit)
	}
	if got.Key != "sk-alpha" {
		t.Fatalf("expected key value to remain unchanged when patch.Key is empty, got %q", got.Key)
	}

	if found := FindApiKeyByValue("sk-alpha"); found == nil || found.ID != created.ID {
		t.Fatalf("FindApiKeyByValue should locate the entry")
	}
	if found := FindApiKeyByValue("does-not-exist"); found != nil {
		t.Fatalf("FindApiKeyByValue should return nil for unknown keys")
	}

	if err := DeleteApiKey(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := GetApiKeyEntry(created.ID); got != nil {
		t.Fatalf("expected entry to be removed")
	}
	if len(ListApiKeys()) != 0 {
		t.Fatalf("expected list to be empty after delete")
	}
}

func TestRecordApiKeyUsageConcurrent(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "race", Key: "sk-race", Enabled: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	const goroutines = 16
	const perGoroutine = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)

	var failures int32
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := RecordApiKeyUsage(created.ID, 7, 0, 0.5); err != nil {
					atomic.AddInt32(&failures, 1)
					return
				}
			}
		}()
	}
	wg.Wait()

	if failures != 0 {
		t.Fatalf("RecordApiKeyUsage encountered %d errors", failures)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil {
		t.Fatalf("entry missing after concurrent updates")
	}
	expectedTokens := int64(goroutines * perGoroutine * 7)
	expectedCredits := float64(goroutines*perGoroutine) * 0.5
	expectedRequests := int64(goroutines * perGoroutine)
	if got.TokensUsed != expectedTokens {
		t.Fatalf("TokensUsed mismatch: got %d want %d", got.TokensUsed, expectedTokens)
	}
	if got.CreditsUsed != expectedCredits {
		t.Fatalf("CreditsUsed mismatch: got %v want %v", got.CreditsUsed, expectedCredits)
	}
	if got.RequestsCount != expectedRequests {
		t.Fatalf("RequestsCount mismatch: got %d want %d", got.RequestsCount, expectedRequests)
	}
}

func TestResetApiKeyUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "reset", Key: "sk-reset", Enabled: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 100, 0, 1.5); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := ResetApiKeyUsage(created.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil {
		t.Fatalf("entry missing")
	}
	if got.TokensUsed != 0 || got.CreditsUsed != 0 || got.RequestsCount != 0 {
		t.Fatalf("expected counters to be zeroed, got %+v", got)
	}
	if got.UsageResetAt == 0 {
		t.Fatalf("expected usage reset timestamp")
	}
	if got.UsagePeriod != time.Now().UTC().Format(usagePeriodFormat) {
		t.Fatalf("expected reset to retain current UTC month, got %q", got.UsagePeriod)
	}
	if len(got.DailyUsage) != 1 || got.DailyUsage[0].TotalTokens != 100 {
		t.Fatalf("expected reset to preserve daily history, got %+v", got.DailyUsage)
	}
}

func TestEnsureApiKeyMonthlyResetUTCMonthBoundaryIncludesUnlimitedKey(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	lastUsed := time.Date(2026, time.September, 15, 4, 0, 0, 0, time.UTC).Unix()
	created, err := AddApiKey(ApiKeyEntry{
		Name: "boundary", Key: "sk-boundary", Enabled: true, UsagePeriod: "2026-09",
		TokenLimit: 0, CreditLimit: 0,
		TokensUsed: 100, CreditsUsed: 2.5, RequestsCount: 4, LastUsedAt: lastUsed,
		DailyUsage: []ApiKeyDailyUsage{{Date: "2026-09-15", Requests: 4, TotalTokens: 100, Credits: 2.5}},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if created.TokenLimit != 0 || created.CreditLimit != 0 {
		t.Fatalf("test key must be unlimited, got token=%d credit=%v", created.TokenLimit, created.CreditLimit)
	}

	local := time.FixedZone("UTC+08", 8*60*60)
	before := time.Date(2026, time.October, 1, 7, 59, 59, 0, local)
	if err := EnsureApiKeyMonthlyResetAt(before); err != nil {
		t.Fatalf("ensure before boundary: %v", err)
	}
	if got := GetApiKeyEntry(created.ID); got.TokensUsed != 100 || got.UsagePeriod != "2026-09" {
		t.Fatalf("quota reset before UTC boundary: %+v", got)
	}

	atBoundary := time.Date(2026, time.October, 1, 8, 0, 0, 0, local)
	if err := EnsureApiKeyMonthlyResetAt(atBoundary); err != nil {
		t.Fatalf("ensure at boundary: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got.TokensUsed != 0 || got.CreditsUsed != 0 || got.RequestsCount != 0 {
		t.Fatalf("expected counters reset at UTC boundary: %+v", got)
	}
	if got.UsagePeriod != "2026-10" || got.UsageResetAt != time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("unexpected period metadata: %+v", got)
	}
	if got.LastUsedAt != lastUsed || len(got.DailyUsage) != 1 || got.DailyUsage[0].TotalTokens != 100 {
		t.Fatalf("monthly reset changed permanent history: %+v", got)
	}
}

func TestRecordApiKeyUsageResetsAndAddsAcrossMonth(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{
		Name: "rollover", Key: "sk-rollover", Enabled: true, UsagePeriod: "2026-09",
		TokensUsed: 999, CreditsUsed: 9, RequestsCount: 9,
		DailyUsage: []ApiKeyDailyUsage{{Date: "2026-09-30", Requests: 9, TotalTokens: 999, Credits: 9}},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	now := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	if err := recordApiKeyUsageAt(created.ID, "gpt-5", 7, 3, 0.25, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got.UsagePeriod != "2026-10" || got.TokensUsed != 10 || got.CreditsUsed != 0.25 || got.RequestsCount != 1 {
		t.Fatalf("rollover did not retain only new request: %+v", got)
	}
	if len(got.DailyUsage) != 2 || got.DailyUsage[0].Date != "2026-09-30" || got.DailyUsage[1].Date != "2026-10-01" {
		t.Fatalf("expected history across rollover: %+v", got.DailyUsage)
	}
}

func TestApiKeyMonthlyMigrationUsesLedgerUnlessAlreadyReset(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	monthStart := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	cfgLock.Lock()
	cfg.ApiKeys = []ApiKeyEntry{
		{ID: "ledger", Key: "sk-ledger", Enabled: true, TokensUsed: 999, CreditsUsed: 9, RequestsCount: 9, DailyUsage: []ApiKeyDailyUsage{
			{Date: "2026-08-31", Requests: 8, TotalTokens: 800, Credits: 8},
			{Date: "2026-09-02", Requests: 2, TotalTokens: 20, Credits: 0.5},
			{Date: "2026-09-03", Requests: 3, TotalTokens: 30, Credits: 0.75},
		}},
		{ID: "manual", Key: "sk-manual", Enabled: true, TokensUsed: 7, CreditsUsed: 0.7, RequestsCount: 1, UsageResetAt: monthStart.Add(12 * time.Hour).Unix(), DailyUsage: []ApiKeyDailyUsage{
			{Date: "2026-09-01", Requests: 20, TotalTokens: 200, Credits: 2},
		}},
	}
	if err := saveLocked(); err != nil {
		cfgLock.Unlock()
		t.Fatalf("seed legacy entries: %v", err)
	}
	cfgLock.Unlock()

	if err := EnsureApiKeyMonthlyResetAt(monthStart.AddDate(0, 0, 5)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ledger := GetApiKeyEntry("ledger")
	if ledger.UsagePeriod != "2026-09" || ledger.TokensUsed != 50 || ledger.CreditsUsed != 1.25 || ledger.RequestsCount != 5 {
		t.Fatalf("legacy counters not rebuilt from current ledger: %+v", ledger)
	}
	manual := GetApiKeyEntry("manual")
	if manual.UsagePeriod != "2026-09" || manual.TokensUsed != 7 || manual.CreditsUsed != 0.7 || manual.RequestsCount != 1 {
		t.Fatalf("same-month manual reset overwritten: %+v", manual)
	}
}

func TestApiKeyMonthlyMigrationPersistsAcrossReload(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	now := time.Now().UTC()
	month := now.Format(usagePeriodFormat)
	day := now.Format("2006-01-02")
	seed := Config{
		Password: "p",
		Port:     8080,
		Host:     "0.0.0.0",
		ApiKeys: []ApiKeyEntry{{
			ID: "legacy-monthly", Key: "sk-legacy-monthly", Enabled: true,
			TokensUsed: 999, CreditsUsed: 9, RequestsCount: 9,
			DailyUsage: []ApiKeyDailyUsage{{Date: day, Requests: 2, TotalTokens: 42, Credits: 0.75}},
		}},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy config: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("migrate on load: %v", err)
	}
	assertMigrated := func(label string) {
		t.Helper()
		got := GetApiKeyEntry("legacy-monthly")
		if got == nil || got.UsagePeriod != month || got.TokensUsed != 42 || got.CreditsUsed != 0.75 || got.RequestsCount != 2 {
			t.Fatalf("%s: unexpected migrated quota: %+v", label, got)
		}
		if len(got.DailyUsage) != 1 || got.DailyUsage[0].TotalTokens != 42 {
			t.Fatalf("%s: migration changed ledger: %+v", label, got.DailyUsage)
		}
	}
	assertMigrated("first load")

	if err := Init(cfgFile); err != nil {
		t.Fatalf("reload migrated config: %v", err)
	}
	assertMigrated("reload")
}

func TestApiKeyDailyUsageUTCAndSnapshot(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "daily", Key: "sk-daily", Enabled: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	// These instants are different local dates but the same UTC date.
	utcDate := time.Date(2026, time.September, 3, 0, 30, 0, 0, time.UTC)
	if err := recordApiKeyUsageAt(created.ID, "claude-sonnet-4", 4, 6, 0.125, utcDate); err != nil {
		t.Fatalf("record first: %v", err)
	}
	if err := recordApiKeyUsageAt(created.ID, "gpt-5", 1, 2, 0.375, utcDate.Add(23*time.Hour)); err != nil {
		t.Fatalf("record same date: %v", err)
	}
	if err := recordApiKeyUsageAt(created.ID, "", 8, 0, 0.5, utcDate.Add(48*time.Hour)); err != nil {
		t.Fatalf("record later date: %v", err)
	}

	got := GetApiKeyEntry(created.ID)
	if got == nil || len(got.DailyUsage) != 2 {
		t.Fatalf("expected two daily records, got %+v", got)
	}
	if got.DailyUsage[0].Date != "2026-09-03" || got.DailyUsage[1].Date != "2026-09-05" {
		t.Fatalf("expected UTC sorted dates, got %+v", got.DailyUsage)
	}
	day := got.DailyUsage[0]
	if day.Requests != 2 || day.InputTokens != 5 || day.OutputTokens != 8 || day.TotalTokens != 13 || day.Credits != 0.5 {
		t.Fatalf("unexpected daily aggregate: %+v", day)
	}

	// Two models on the first UTC day, sorted by model name.
	if len(day.Models) != 2 || day.Models[0].Model != "claude-sonnet-4" || day.Models[1].Model != "gpt-5" {
		t.Fatalf("unexpected model breakdown: %+v", day.Models)
	}
	if day.Models[0].TotalTokens != 10 || day.Models[1].TotalTokens != 3 {
		t.Fatalf("unexpected per-model tokens: %+v", day.Models)
	}
	if sum := day.Models[0].TotalTokens + day.Models[1].TotalTokens; sum != day.TotalTokens {
		t.Fatalf("model tokens %d do not sum to day total %d", sum, day.TotalTokens)
	}
	// A blank model label lands in the unknown bucket.
	if models := got.DailyUsage[1].Models; len(models) != 1 || models[0].Model != unknownModelName {
		t.Fatalf("expected unknown model bucket, got %+v", models)
	}

	// Model rows must be deep-copied out of the persisted ledger.
	got.DailyUsage[0].Models[0].TotalTokens = 4242
	if fresh := GetApiKeyEntry(created.ID); fresh.DailyUsage[0].Models[0].TotalTokens != 10 {
		t.Fatalf("model usage snapshot leaked internal state: %+v", fresh.DailyUsage[0].Models)
	}

	// Returned entries must not expose the persisted slice backing array.
	got.DailyUsage[0].TotalTokens = 999
	got.DailyUsage = append(got.DailyUsage, ApiKeyDailyUsage{Date: "2099-01-01"})
	snapshot := GetApiKeyEntry(created.ID)
	if snapshot == nil || snapshot.DailyUsage[0].TotalTokens != 13 || len(snapshot.DailyUsage) != 2 {
		t.Fatalf("daily usage snapshot leaked internal state: %+v", snapshot)
	}
}

func TestApiKeyUsagePersistsAndReloads(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "reload", Key: "sk-reload", Enabled: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 11, 7, 0.125); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil || len(got.DailyUsage) != 1 {
		t.Fatalf("expected usage to survive reload, got %+v", got)
	}
	if got.DailyUsage[0].InputTokens != 11 || got.DailyUsage[0].OutputTokens != 7 || got.DailyUsage[0].TotalTokens != 18 || got.DailyUsage[0].Credits != 0.125 {
		t.Fatalf("unexpected reloaded usage: %+v", got.DailyUsage[0])
	}
}

func TestApiKeyOverLimit(t *testing.T) {
	tests := []struct {
		name       string
		entry      ApiKeyEntry
		wantToken  bool
		wantCredit bool
	}{
		{"unlimited", ApiKeyEntry{TokensUsed: 100, CreditsUsed: 5}, false, false},
		{"under token limit", ApiKeyEntry{TokenLimit: 200, TokensUsed: 100}, false, false},
		{"at token limit", ApiKeyEntry{TokenLimit: 100, TokensUsed: 100}, true, false},
		{"over token limit", ApiKeyEntry{TokenLimit: 100, TokensUsed: 150}, true, false},
		{"over credit limit", ApiKeyEntry{CreditLimit: 1, CreditsUsed: 2}, false, true},
		{"both over", ApiKeyEntry{TokenLimit: 1, TokensUsed: 2, CreditLimit: 1, CreditsUsed: 2}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotT, gotC := ApiKeyOverLimit(tc.entry)
			if gotT != tc.wantToken || gotC != tc.wantCredit {
				t.Fatalf("ApiKeyOverLimit(%+v) = (%v,%v), want (%v,%v)",
					tc.entry, gotT, gotC, tc.wantToken, tc.wantCredit)
			}
		})
	}
}

func TestMaskApiKey(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"tiny", "****"},
		{"short", "sh****rt"},
		{"sk-1234567890", "sk-123****7890"},
	}
	for _, tc := range tests {
		if got := MaskApiKey(tc.in); got != tc.want {
			t.Fatalf("MaskApiKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGenerateApiKeyValueIsUnique(t *testing.T) {
	a := GenerateApiKeyValue()
	b := GenerateApiKeyValue()
	if a == b {
		t.Fatalf("expected unique generated keys, got identical %q", a)
	}
	if len(a) < 10 {
		t.Fatalf("expected non-trivial key length, got %q", a)
	}
}
