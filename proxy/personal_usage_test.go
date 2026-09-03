package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPersonalUsageRequiresConfiguredKeyWhenGlobalAuthIsOff(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: "personal", Key: "sk-personal", Enabled: true}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	h := &Handler{}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", recorder.Code, recorder.Body.String())
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cache)
	}
}

func TestPersonalUsageAcceptsBearerAndXAPIKeyBeyondQuota(t *testing.T) {
	mustInitConfig(t)
	created, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "limited", Key: "sk-limited", Enabled: true, TokenLimit: 5, CreditLimit: 0.5,
	})
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if err := config.RecordApiKeyUsage(created.ID, 4, 6, 0.75); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	for _, header := range []struct{ name, value string }{
		{"Authorization", "Bearer sk-limited"},
		{"X-Api-Key", "sk-limited"},
	} {
		h := &Handler{}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/usage?days=1", nil)
		request.Header.Set(header.name, header.value)
		h.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", header.name, recorder.Code, recorder.Body.String())
		}
		var response personalUsageResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if response.QuotaUsage.TokensUsed != 10 || response.QuotaUsage.CreditsUsed != 0.75 {
			t.Fatalf("unexpected quota usage: %+v", response.QuotaUsage)
		}
		if response.APIKey.KeyMasked == "sk-limited" || strings.Contains(recorder.Body.String(), `"key":"sk-limited"`) {
			t.Fatalf("response leaked cleartext key: %s", recorder.Body.String())
		}
	}
}

func TestPersonalUsageReportsOnlyAggregateAccountPoolCredit(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: "pool-view", Key: "sk-pool-view", Enabled: true}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	accounts := []config.Account{
		{ID: "active-a", Email: "first@example.com", Enabled: true, UsageCurrent: 40, UsageLimit: 100},
		{ID: "active-b", Email: "second@example.com", Enabled: true, UsageCurrent: 120, UsageLimit: 100},
		{ID: "disabled", Email: "disabled@example.com", Enabled: false, UsageCurrent: 5, UsageLimit: 20},
		{ID: "unlimited", Email: "unlimited@example.com", Enabled: true, UsageCurrent: 10, UsageLimit: 0},
	}
	for _, account := range accounts {
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("add account %s: %v", account.ID, err)
		}
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/usage?days=1", nil)
	request.Header.Set("Authorization", "Bearer sk-pool-view")
	(&Handler{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response personalUsageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.AccountPool == nil {
		t.Fatal("expected account-pool aggregate")
	}
	if got := *response.AccountPool; got.CreditsUsed != 160 || got.CreditLimit != 200 || got.CreditsRemaining != 40 {
		t.Fatalf("unexpected account-pool aggregate: %+v", got)
	}
	body := recorder.Body.String()
	for _, private := range []string{"active-a", "active-b", "first@example.com", "second@example.com"} {
		if strings.Contains(body, private) {
			t.Fatalf("account-pool response leaked %q: %s", private, body)
		}
	}
}

func TestAggregateAccountPoolUsageOmitsPoolWithoutFiniteQuota(t *testing.T) {
	got := aggregateAccountPoolUsage([]config.Account{
		{Enabled: true, UsageCurrent: 10},
		{Enabled: false, UsageCurrent: 1, UsageLimit: 20},
	})
	if got != nil {
		t.Fatalf("expected no aggregate without enabled finite quota, got %+v", got)
	}
}

func TestPersonalUsageResetsPreviousUTCMonthUsageForUnlimitedKey(t *testing.T) {
	mustInitConfig(t)
	previousMonth := time.Now().UTC().AddDate(0, -1, 0).Format(usageMonthLayout)
	_, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "monthly-usage", Key: "sk-monthly-usage", Enabled: true,
		TokenLimit: 0, CreditLimit: 0,
		UsagePeriod: previousMonth, TokensUsed: 9, CreditsUsed: 1.5, RequestsCount: 3,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/usage?month=current", nil)
	request.Header.Set("Authorization", "Bearer sk-monthly-usage")
	(&Handler{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response personalUsageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.QuotaUsage.TokensUsed != 0 || response.QuotaUsage.CreditsUsed != 0 || response.QuotaUsage.RequestsUsed != 0 {
		t.Fatalf("personal usage returned previous-month counters for unlimited key: %+v", response.QuotaUsage)
	}
	if response.QuotaUsage.TokenLimit != 0 || response.QuotaUsage.CreditLimit != 0 {
		t.Fatalf("expected unlimited key limits to remain zero: %+v", response.QuotaUsage)
	}
}

func TestPersonalUsageRejectsDisabledKey(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: "disabled", Key: "sk-disabled", Enabled: false}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	h := &Handler{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	request.Header.Set("Authorization", "Bearer sk-disabled")
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestParseUsageRange(t *testing.T) {
	now := time.Date(2026, time.September, 3, 18, 30, 0, 0, time.FixedZone("test", 8*60*60))
	// The default window is the natural UTC month, clamped at today.
	window, err := parseUsageRange(map[string][]string{}, now)
	if err != nil {
		t.Fatalf("default range: %v", err)
	}
	if window.kind != "month" || window.month != "2026-09" {
		t.Fatalf("kind = %q month = %q, want month/2026-09", window.kind, window.month)
	}
	if got, want := window.start.Format(usageDateLayout), "2026-09-01"; got != want {
		t.Fatalf("start = %s, want %s", got, want)
	}
	if got, want := window.end.Format(usageDateLayout), "2026-09-03"; got != want {
		t.Fatalf("end = %s, want %s", got, want)
	}
	// monthEnd keeps the full natural month even while the month is running.
	if got, want := window.monthEnd.Format(usageDateLayout), "2026-09-30"; got != want {
		t.Fatalf("monthEnd = %s, want %s", got, want)
	}
	if window.complete {
		t.Fatal("in-progress month must not be reported complete")
	}
	if got, want := window.resetsAt.UTC().Format(time.RFC3339), "2026-10-01T00:00:00Z"; got != want {
		t.Fatalf("resetsAt = %s, want %s", got, want)
	}

	// An explicit past month covers the whole month and is complete.
	past, err := parseUsageRange(map[string][]string{"month": {"2026-02"}}, now)
	if err != nil {
		t.Fatalf("past month: %v", err)
	}
	if got, want := past.end.Format(usageDateLayout), "2026-02-28"; got != want {
		t.Fatalf("past month end = %s, want %s", got, want)
	}
	if !past.complete || past.days() != 28 {
		t.Fatalf("past month should be complete with 28 days, got complete=%v days=%d", past.complete, past.days())
	}

	// Explicit days keeps the previous rolling behavior.
	rolling, err := parseUsageRange(map[string][]string{"days": {"30"}}, now)
	if err != nil {
		t.Fatalf("rolling range: %v", err)
	}
	if got, want := rolling.start.Format(usageDateLayout), "2026-08-05"; got != want {
		t.Fatalf("rolling start = %s, want %s", got, want)
	}
	if got, want := rolling.end.Format(usageDateLayout), "2026-09-03"; got != want {
		t.Fatalf("rolling end = %s, want %s", got, want)
	}

	bad := []map[string][]string{
		{"days": {"0"}},
		{"days": {"367"}},
		{"days": {"7"}, "start": {"2026-09-01"}, "end": {"2026-09-03"}},
		{"start": {"2026-09-01"}},
		{"start": {"2026-09-03"}, "end": {"2026-09-01"}},
		{"start": {"2025-01-01"}, "end": {"2026-09-03"}},
		{"month": {"2026-13"}},
		{"month": {"2026-9"}},
		{"month": {"2027-01"}},
		{"month": {"2026-09"}, "days": {"7"}},
	}
	for _, values := range bad {
		if _, err := parseUsageRange(values, now); err == nil {
			t.Fatalf("expected invalid range for %#v", values)
		}
	}
}

func TestBuildPersonalUsageResponseZeroFillsDailyRange(t *testing.T) {
	entry := config.ApiKeyEntry{
		ID: "id", Name: "name", Key: "sk-a-long-personal-key", Enabled: true,
		DailyUsage: []config.ApiKeyDailyUsage{{
			Date: "2026-09-02", Requests: 2, InputTokens: 10, OutputTokens: 4, TotalTokens: 14, Credits: 0.25,
			Models: []config.ApiKeyModelUsage{
				{Model: "claude-sonnet-4", Requests: 1, InputTokens: 8, OutputTokens: 2, TotalTokens: 10, Credits: 0.2},
				{Model: "gpt-5", Requests: 1, InputTokens: 2, OutputTokens: 2, TotalTokens: 4, Credits: 0.05},
			},
		}},
	}
	window := usageWindow{
		start: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		end:   time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC),
		kind:  "custom",
	}
	response := buildPersonalUsageResponse(entry, window)
	if len(response.Daily) != 3 {
		t.Fatalf("daily length = %d, want 3", len(response.Daily))
	}
	if response.Daily[0].TotalTokens != 0 || response.Daily[1].TotalTokens != 14 || response.Daily[2].TotalTokens != 0 {
		t.Fatalf("unexpected zero fill: %+v", response.Daily)
	}
	if response.Totals.TotalTokens != 14 || response.Totals.Credits != 0.25 || response.HistoryAvailableFrom != "2026-09-02" {
		t.Fatalf("unexpected aggregate: %+v history=%q", response.Totals, response.HistoryAvailableFrom)
	}
	if response.QuotaUsage.TokensRemaining != nil || response.QuotaUsage.CreditsRemaining != nil {
		t.Fatalf("unlimited quotas should return null remaining values: %+v", response.QuotaUsage)
	}

	// Window-level model rollup is ranked by tokens and shares sum to 1.
	if len(response.Models) != 2 || response.Models[0].Model != "claude-sonnet-4" {
		t.Fatalf("unexpected model rollup: %+v", response.Models)
	}
	if share := response.Models[0].Share + response.Models[1].Share; share < 0.999 || share > 1.001 {
		t.Fatalf("model shares sum to %v, want 1", share)
	}
	if got := response.Daily[1].Models; len(got) != 2 || got[0].TotalTokens != 10 {
		t.Fatalf("unexpected per-day model rows: %+v", got)
	}
	// Days with no usage carry no model rows.
	if response.Daily[0].Models != nil {
		t.Fatalf("empty day should omit models, got %+v", response.Daily[0].Models)
	}
}

// Ledger days written before model attribution must still account for tokens.
func TestBuildPersonalUsageResponseBucketsLegacyDaysAsUnknown(t *testing.T) {
	entry := config.ApiKeyEntry{
		ID: "id", Key: "sk-a-long-personal-key", Enabled: true,
		DailyUsage: []config.ApiKeyDailyUsage{{
			Date: "2026-09-02", Requests: 3, InputTokens: 20, OutputTokens: 5, TotalTokens: 25, Credits: 0.5,
		}},
	}
	window := usageWindow{
		start: time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC),
		end:   time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC),
		kind:  "custom",
	}
	response := buildPersonalUsageResponse(entry, window)
	if len(response.Models) != 1 || response.Models[0].Model != unknownUsageModel {
		t.Fatalf("unexpected legacy rollup: %+v", response.Models)
	}
	if response.Models[0].TotalTokens != 25 || response.Models[0].Share != 1 {
		t.Fatalf("legacy bucket must carry the full window: %+v", response.Models[0])
	}
}

// A month query reports natural month bounds and the next reset instant.
func TestPersonalUsageMonthResponseShape(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{
		Name: "month", Key: "sk-month", Enabled: true, CreditLimit: 200,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	h := &Handler{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/usage?month=2026-02", nil)
	request.Header.Set("Authorization", "Bearer sk-month")
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var response personalUsageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Range.Kind != "month" || response.Range.Month != "2026-02" {
		t.Fatalf("unexpected range: %+v", response.Range)
	}
	if response.Range.Start != "2026-02-01" || response.Range.End != "2026-02-28" || response.Range.Days != 28 {
		t.Fatalf("unexpected month bounds: %+v", response.Range)
	}
	if response.Range.ResetsAt != time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("unexpected resetsAt: %d", response.Range.ResetsAt)
	}
	if len(response.Daily) != response.Range.Days {
		t.Fatalf("daily length %d does not match range days %d", len(response.Daily), response.Range.Days)
	}
}
