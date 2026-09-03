package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	defaultUsageDays = 30
	maxUsageDays     = 366
	usageDateLayout  = "2006-01-02"
	usageMonthLayout = "2006-01"
	// unknownUsageModel labels usage that carries no model attribution, which
	// includes ledger days written before per-model accounting existed.
	unknownUsageModel = "unknown"
)

type personalUsageTotals struct {
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	TotalTokens  int64   `json:"totalTokens"`
	Credits      float64 `json:"credits"`
}

// personalUsageModel is one model's slice of a window, with Share as the
// fraction of the window's total tokens (0 when the window has no tokens).
type personalUsageModel struct {
	Model string `json:"model"`
	personalUsageTotals
	Share float64 `json:"share"`
}

type personalUsageDay struct {
	Date string `json:"date"`
	personalUsageTotals
	Models []personalUsageModel `json:"models,omitempty"`
}

type personalUsageRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Days  int    `json:"days"`
	// Kind is "month" for natural UTC calendar months, otherwise "days".
	Kind string `json:"kind"`
	// Month is the YYYY-MM label and MonthStart/MonthEnd the full natural
	// month bounds; End may be earlier than MonthEnd for the current month.
	Month      string `json:"month,omitempty"`
	MonthStart string `json:"monthStart,omitempty"`
	MonthEnd   string `json:"monthEnd,omitempty"`
	// ResetsAt is the first instant of the next UTC month (Unix seconds).
	ResetsAt int64 `json:"resetsAt,omitempty"`
	// Complete is false while the month is still in progress.
	Complete bool `json:"complete"`
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

// personalAccountPoolUsage exposes only aggregate main-credit capacity. It
// deliberately carries no account identifiers or per-account usage rows.
type personalAccountPoolUsage struct {
	CreditsUsed      float64 `json:"creditsUsed"`
	CreditLimit      float64 `json:"creditLimit"`
	CreditsRemaining float64 `json:"creditsRemaining"`
}

type personalUsageResponse struct {
	APIKey               personalUsageAPIKey       `json:"apiKey"`
	Timezone             string                    `json:"timezone"`
	Range                personalUsageRange        `json:"range"`
	HistoryAvailableFrom string                    `json:"historyAvailableFrom,omitempty"`
	QuotaUsage           personalQuotaUsage        `json:"quotaUsage"`
	AccountPool          *personalAccountPoolUsage `json:"accountPool,omitempty"`
	Totals               personalUsageTotals       `json:"totals"`
	// Models is the per-model rollup for the whole window, ordered by token
	// volume descending so the largest consumer is first.
	Models []personalUsageModel `json:"models"`
	Daily  []personalUsageDay   `json:"daily"`
}

func aggregateAccountPoolUsage(accounts []config.Account) *personalAccountPoolUsage {
	var used, limit float64
	for _, account := range accounts {
		if !account.Enabled || account.UsageLimit <= 0 {
			continue
		}
		limit += account.UsageLimit
		current := account.UsageCurrent
		if current < 0 {
			current = 0
		}
		used += current
	}
	if limit <= 0 {
		return nil
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return &personalAccountPoolUsage{CreditsUsed: used, CreditLimit: limit, CreditsRemaining: remaining}
}

// usageWindow describes one resolved reporting window.
type usageWindow struct {
	start      time.Time
	end        time.Time
	kind       string
	month      string
	monthStart time.Time
	monthEnd   time.Time
	resetsAt   time.Time
	complete   bool
}

// days is the inclusive number of UTC days covered by the window.
func (w usageWindow) days() int {
	return int(w.end.Sub(w.start).Hours()/24) + 1
}

// parseUsageMonth resolves a month selector into a natural UTC calendar month.
// "current" (or an empty value) means the month containing now. The window ends
// at the current UTC day for an in-progress month so the response never reports
// future dates, while monthStart/monthEnd always carry the full month bounds.
func parseUsageMonth(text string, now time.Time) (usageWindow, error) {
	today := now.UTC().Truncate(24 * time.Hour)
	var first time.Time
	switch strings.ToLower(text) {
	case "", "current", "now", "this":
		first = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		parsed, err := time.ParseInLocation(usageMonthLayout, text, time.UTC)
		if err != nil {
			return usageWindow{}, fmt.Errorf("month must be YYYY-MM or current")
		}
		first = parsed
	}
	next := first.AddDate(0, 1, 0)
	last := next.AddDate(0, 0, -1)
	if first.After(today) {
		return usageWindow{}, fmt.Errorf("month must not be in the future")
	}
	window := usageWindow{
		start: first, end: last, kind: "month",
		month: first.Format(usageMonthLayout), monthStart: first, monthEnd: last,
		resetsAt: next, complete: true,
	}
	if last.After(today) {
		window.end = today
		window.complete = false
	}
	return window, nil
}

func parseUsageRange(values map[string][]string, now time.Time) (usageWindow, error) {
	daysText := strings.TrimSpace(firstQueryValue(values, "days"))
	startText := strings.TrimSpace(firstQueryValue(values, "start"))
	endText := strings.TrimSpace(firstQueryValue(values, "end"))
	monthText := strings.TrimSpace(firstQueryValue(values, "month"))
	_, monthPresent := values["month"]

	if monthPresent && (daysText != "" || startText != "" || endText != "") {
		return usageWindow{}, fmt.Errorf("month cannot be combined with days, start, or end")
	}
	if daysText != "" && (startText != "" || endText != "") {
		return usageWindow{}, fmt.Errorf("days cannot be combined with start or end")
	}
	if (startText == "") != (endText == "") {
		return usageWindow{}, fmt.Errorf("start and end must be provided together")
	}
	// A natural UTC calendar month is the default window; explicit days or an
	// explicit start/end pair keep the previous rolling behavior.
	if monthPresent || (daysText == "" && startText == "") {
		return parseUsageMonth(monthText, now)
	}
	if startText != "" {
		start, err := time.Parse(usageDateLayout, startText)
		if err != nil {
			return usageWindow{}, fmt.Errorf("start must be a valid YYYY-MM-DD date")
		}
		end, err := time.Parse(usageDateLayout, endText)
		if err != nil {
			return usageWindow{}, fmt.Errorf("end must be a valid YYYY-MM-DD date")
		}
		days := int(end.Sub(start).Hours()/24) + 1
		if days < 1 {
			return usageWindow{}, fmt.Errorf("end must not be before start")
		}
		if days > maxUsageDays {
			return usageWindow{}, fmt.Errorf("date range cannot exceed %d days", maxUsageDays)
		}
		return usageWindow{start: start, end: end, kind: "custom"}, nil
	}

	days := defaultUsageDays
	if daysText != "" {
		if _, err := fmt.Sscanf(daysText, "%d", &days); err != nil || fmt.Sprintf("%d", days) != daysText {
			return usageWindow{}, fmt.Errorf("days must be an integer from 1 to %d", maxUsageDays)
		}
	}
	if days < 1 || days > maxUsageDays {
		return usageWindow{}, fmt.Errorf("days must be an integer from 1 to %d", maxUsageDays)
	}
	end := now.UTC().Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -(days - 1))
	return usageWindow{start: start, end: end, kind: "rolling"}, nil
}

func firstQueryValue(values map[string][]string, key string) string {
	if list := values[key]; len(list) > 0 {
		return list[0]
	}
	return ""
}

func authenticatePersonalUsage(r *http.Request) (*config.ApiKeyEntry, int, string) {
	if err := config.EnsureApiKeyMonthlyReset(); err != nil {
		return nil, http.StatusInternalServerError, "failed to refresh API key quota period"
	}
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

func buildPersonalUsageResponse(entry config.ApiKeyEntry, window usageWindow) personalUsageResponse {
	byDate := make(map[string]config.ApiKeyDailyUsage, len(entry.DailyUsage))
	historyFrom := ""
	for _, usage := range entry.DailyUsage {
		byDate[usage.Date] = usage
		if historyFrom == "" || usage.Date < historyFrom {
			historyFrom = usage.Date
		}
	}

	start, end := window.start, window.end
	days := window.days()
	daily := make([]personalUsageDay, 0, days)
	var totals personalUsageTotals
	windowModels := make(map[string]*personalUsageTotals)
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
		day.Models = dayModels(usage)
		for _, model := range legacyAwareModels(usage) {
			agg := windowModels[model.Model]
			if agg == nil {
				agg = &personalUsageTotals{}
				windowModels[model.Model] = agg
			}
			agg.Requests += model.Requests
			agg.InputTokens += model.InputTokens
			agg.OutputTokens += model.OutputTokens
			agg.TotalTokens += model.TotalTokens
			agg.Credits += model.Credits
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

	reportedRange := personalUsageRange{
		Start: start.Format(usageDateLayout), End: end.Format(usageDateLayout), Days: days,
		Kind: window.kind, Complete: window.complete,
	}
	if window.kind == "month" {
		reportedRange.Month = window.month
		reportedRange.MonthStart = window.monthStart.Format(usageDateLayout)
		reportedRange.MonthEnd = window.monthEnd.Format(usageDateLayout)
		reportedRange.ResetsAt = window.resetsAt.Unix()
	}

	return personalUsageResponse{
		APIKey:   personalUsageAPIKey{ID: entry.ID, Name: entry.Name, KeyMasked: config.MaskApiKey(entry.Key), CreatedAt: entry.CreatedAt, LastUsedAt: entry.LastUsedAt},
		Timezone: "UTC", Range: reportedRange,
		HistoryAvailableFrom: historyFrom, QuotaUsage: quota, Totals: totals,
		Models: rankModels(windowModels, totals.TotalTokens), Daily: daily,
	}
}

// dayModels converts a stored day's per-model rows into response rows, with the
// share computed against that day's own token total.
func dayModels(usage config.ApiKeyDailyUsage) []personalUsageModel {
	rows := legacyAwareModels(usage)
	if len(rows) == 0 {
		return nil
	}
	byModel := make(map[string]*personalUsageTotals, len(rows))
	for _, model := range rows {
		byModel[model.Model] = &personalUsageTotals{
			Requests: model.Requests, InputTokens: model.InputTokens, OutputTokens: model.OutputTokens,
			TotalTokens: model.TotalTokens, Credits: model.Credits,
		}
	}
	return rankModels(byModel, usage.TotalTokens)
}

// legacyAwareModels returns a day's per-model rows. Ledger days written before
// model attribution existed carry usage but no model rows; those are reported
// as a single unknown bucket so model shares still sum to the day's totals.
func legacyAwareModels(usage config.ApiKeyDailyUsage) []config.ApiKeyModelUsage {
	if len(usage.Models) > 0 {
		return usage.Models
	}
	if usage.Requests == 0 && usage.TotalTokens == 0 && usage.Credits == 0 {
		return nil
	}
	return []config.ApiKeyModelUsage{{
		Model: unknownUsageModel, Requests: usage.Requests, InputTokens: usage.InputTokens,
		OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens, Credits: usage.Credits,
	}}
}

// rankModels orders models by token volume descending, then by name so equal
// volumes stay stable, and fills in each model's share of totalTokens.
func rankModels(byModel map[string]*personalUsageTotals, totalTokens int64) []personalUsageModel {
	models := make([]personalUsageModel, 0, len(byModel))
	for name, agg := range byModel {
		row := personalUsageModel{Model: name, personalUsageTotals: *agg}
		if totalTokens > 0 {
			row.Share = float64(agg.TotalTokens) / float64(totalTokens)
		}
		models = append(models, row)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].TotalTokens != models[j].TotalTokens {
			return models[i].TotalTokens > models[j].TotalTokens
		}
		return models[i].Model < models[j].Model
	})
	return models
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
	window, err := parseUsageRange(r.URL.Query(), time.Now())
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	response := buildPersonalUsageResponse(*entry, window)
	response.AccountPool = aggregateAccountPoolUsage(config.GetAccounts())
	_ = json.NewEncoder(w).Encode(response)
}
