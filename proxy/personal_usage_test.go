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
	start, end, err := parseUsageRange(map[string][]string{}, now)
	if err != nil {
		t.Fatalf("default range: %v", err)
	}
	if got, want := start.Format(usageDateLayout), "2026-08-05"; got != want {
		t.Fatalf("start = %s, want %s", got, want)
	}
	if got, want := end.Format(usageDateLayout), "2026-09-03"; got != want {
		t.Fatalf("end = %s, want %s", got, want)
	}

	bad := []map[string][]string{
		{"days": {"0"}},
		{"days": {"367"}},
		{"days": {"7"}, "start": {"2026-09-01"}, "end": {"2026-09-03"}},
		{"start": {"2026-09-01"}},
		{"start": {"2026-09-03"}, "end": {"2026-09-01"}},
		{"start": {"2025-01-01"}, "end": {"2026-09-03"}},
	}
	for _, values := range bad {
		if _, _, err := parseUsageRange(values, now); err == nil {
			t.Fatalf("expected invalid range for %#v", values)
		}
	}
}

func TestBuildPersonalUsageResponseZeroFillsDailyRange(t *testing.T) {
	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	entry := config.ApiKeyEntry{
		ID: "id", Name: "name", Key: "sk-a-long-personal-key", Enabled: true,
		DailyUsage: []config.ApiKeyDailyUsage{{
			Date: "2026-09-02", Requests: 2, InputTokens: 10, OutputTokens: 4, TotalTokens: 14, Credits: 0.25,
		}},
	}
	response := buildPersonalUsageResponse(entry, start, end)
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
}
