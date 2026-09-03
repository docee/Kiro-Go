package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"
)

const (
	defaultUsageDays = 30
	maxUsageDays     = 366
	usageDateLayout  = "2006-01-02"
)

type personalUsageTotals struct {
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	TotalTokens  int64   `json:"totalTokens"`
	Credits      float64 `json:"credits"`
}

type personalUsageDay struct {
	Date string `json:"date"`
	personalUsageTotals
}

type personalUsageRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Days  int    `json:"days"`
}

type personalUsageAPIKey struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	KeyMasked  string `json:"keyMasked"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`
}

type personalQuotaUsage struct {
	ResetAt          int64    `json:"resetAt,omitempty"`
	RequestsUsed     int64    `json:"requestsUsed"`
	TokensUsed       int64    `json:"tokensUsed"`
	TokenLimit       int64    `json:"tokenLimit"`
	TokensRemaining  *int64   `json:"tokensRemaining"`
	CreditsUsed      float64  `json:"creditsUsed"`
	CreditLimit      float64  `json:"creditLimit"`
	CreditsRemaining *float64 `json:"creditsRemaining"`
}

type personalUsageResponse struct {
	APIKey               personalUsageAPIKey `json:"apiKey"`
	Timezone             string              `json:"timezone"`
	Range                personalUsageRange  `json:"range"`
	HistoryAvailableFrom string              `json:"historyAvailableFrom,omitempty"`
	QuotaUsage           personalQuotaUsage  `json:"quotaUsage"`
	Totals               personalUsageTotals `json:"totals"`
	Daily                []personalUsageDay  `json:"daily"`
}

func parseUsageRange(values map[string][]string, now time.Time) (time.Time, time.Time, error) {
	daysText := strings.TrimSpace(firstQueryValue(values, "days"))
	startText := strings.TrimSpace(firstQueryValue(values, "start"))
	endText := strings.TrimSpace(firstQueryValue(values, "end"))

	if daysText != "" && (startText != "" || endText != "") {
		return time.Time{}, time.Time{}, fmt.Errorf("days cannot be combined with start or end")
	}
	if (startText == "") != (endText == "") {
		return time.Time{}, time.Time{}, fmt.Errorf("start and end must be provided together")
	}
	if startText != "" {
		start, err := time.Parse(usageDateLayout, startText)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("start must be a valid YYYY-MM-DD date")
		}
		end, err := time.Parse(usageDateLayout, endText)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end must be a valid YYYY-MM-DD date")
		}
		days := int(end.Sub(start).Hours()/24) + 1
		if days < 1 {
			return time.Time{}, time.Time{}, fmt.Errorf("end must not be before start")
		}
		if days > maxUsageDays {
			return time.Time{}, time.Time{}, fmt.Errorf("date range cannot exceed %d days", maxUsageDays)
		}
		return start, end, nil
	}

	days := defaultUsageDays
	if daysText != "" {
		if _, err := fmt.Sscanf(daysText, "%d", &days); err != nil || fmt.Sprintf("%d", days) != daysText {
			return time.Time{}, time.Time{}, fmt.Errorf("days must be an integer from 1 to %d", maxUsageDays)
		}
	}
	if days < 1 || days > maxUsageDays {
		return time.Time{}, time.Time{}, fmt.Errorf("days must be an integer from 1 to %d", maxUsageDays)
	}
	end := now.UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -(days - 1))
	return start, end, nil
}

func firstQueryValue(values map[string][]string, key string) string {
	if list := values[key]; len(list) > 0 {
		return list[0]
	}
	return ""
}

func authenticatePersonalUsage(r *http.Request) (*config.ApiKeyEntry, int, string) {
	provided := extractProvidedKey(r)
	if provided == "" {
		return nil, http.StatusUnauthorized, "Invalid or missing API key"
	}
	entry := config.FindApiKeyByValue(provided)
	if entry == nil {
		return nil, http.StatusUnauthorized, "Invalid or missing API key"
	}
	if !entry.Enabled {
		return nil, http.StatusForbidden, "API key disabled"
	}
	return entry, 0, ""
}

func buildPersonalUsageResponse(entry config.ApiKeyEntry, start, end time.Time) personalUsageResponse {
	byDate := make(map[string]config.ApiKeyDailyUsage, len(entry.DailyUsage))
	historyFrom := ""
	for _, usage := range entry.DailyUsage {
		byDate[usage.Date] = usage
		if historyFrom == "" || usage.Date < historyFrom {
			historyFrom = usage.Date
		}
	}

	days := int(end.Sub(start).Hours()/24) + 1
	daily := make([]personalUsageDay, 0, days)
	var totals personalUsageTotals
	for date := start; !date.After(end); date = date.AddDate(0, 0, 1) {
		dateText := date.Format(usageDateLayout)
		usage := byDate[dateText]
		day := personalUsageDay{
			Date: dateText,
			personalUsageTotals: personalUsageTotals{
				Requests: usage.Requests, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
				TotalTokens: usage.TotalTokens, Credits: usage.Credits,
			},
		}
		daily = append(daily, day)
		totals.Requests += day.Requests
		totals.InputTokens += day.InputTokens
		totals.OutputTokens += day.OutputTokens
		totals.TotalTokens += day.TotalTokens
		totals.Credits += day.Credits
	}

	quota := personalQuotaUsage{
		ResetAt: entry.UsageResetAt, RequestsUsed: entry.RequestsCount, TokensUsed: entry.TokensUsed,
		TokenLimit: entry.TokenLimit, CreditsUsed: entry.CreditsUsed, CreditLimit: entry.CreditLimit,
	}
	if entry.TokenLimit > 0 {
		remaining := maxInt64(0, entry.TokenLimit-entry.TokensUsed)
		quota.TokensRemaining = &remaining
	}
	if entry.CreditLimit > 0 {
		remaining := entry.CreditLimit - entry.CreditsUsed
		if remaining < 0 {
			remaining = 0
		}
		quota.CreditsRemaining = &remaining
	}

	return personalUsageResponse{
		APIKey:   personalUsageAPIKey{ID: entry.ID, Name: entry.Name, KeyMasked: config.MaskApiKey(entry.Key), CreatedAt: entry.CreatedAt, LastUsedAt: entry.LastUsedAt},
		Timezone: "UTC", Range: personalUsageRange{Start: start.Format(usageDateLayout), End: end.Format(usageDateLayout), Days: days},
		HistoryAvailableFrom: historyFrom, QuotaUsage: quota, Totals: totals, Daily: daily,
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (h *Handler) handlePersonalUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	entry, status, message := authenticatePersonalUsage(r)
	if entry == nil {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		return
	}
	start, end, err := parseUsageRange(r.URL.Query(), time.Now())
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(buildPersonalUsageResponse(*entry, start, end))
}
