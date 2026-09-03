package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"
)

const usagePeriodFormat = "2006-01"

func usageMonthStart(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func usageMonth(now time.Time) string { return usageMonthStart(now).Format(usagePeriodFormat) }

// EnsureApiKeyMonthlyReset lazily normalizes every API key against the current
// UTC calendar month and persists changes.
func EnsureApiKeyMonthlyReset() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if ensureApiKeyMonthlyResetLocked(time.Now()) {
		return saveLocked()
	}
	return nil
}

// EnsureApiKeyMonthlyResetAt is the deterministic variant used by tests.
func EnsureApiKeyMonthlyResetAt(now time.Time) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if ensureApiKeyMonthlyResetLocked(now) {
		return saveLocked()
	}
	return nil
}

// ensureApiKeyMonthlyResetLocked updates counters in-place. Caller holds cfgLock.
// Empty usagePeriod is the pre-monthly schema: preserve counters when a manual
// reset occurred in this month, otherwise rebuild them from the UTC daily ledger.
func ensureApiKeyMonthlyResetLocked(now time.Time) bool {
	if cfg == nil {
		return false
	}
	month := usageMonth(now)
	monthStart := usageMonthStart(now)
	changed := false
	for i := range cfg.ApiKeys {
		entry := &cfg.ApiKeys[i]
		if entry.UsagePeriod == "" {
			if entry.UsageResetAt > 0 && time.Unix(entry.UsageResetAt, 0).UTC().Format(usagePeriodFormat) == month {
				entry.UsagePeriod = month
				changed = true
				continue
			}
			entry.TokensUsed, entry.CreditsUsed, entry.RequestsCount = monthlyLedgerTotals(entry, month)
			entry.UsagePeriod = month
			entry.UsageResetAt = monthStart.Unix()
			changed = true
			continue
		}
		if entry.UsagePeriod < month {
			entry.TokensUsed = 0
			entry.CreditsUsed = 0
			entry.RequestsCount = 0
			entry.UsagePeriod = month
			entry.UsageResetAt = monthStart.Unix()
			changed = true
		}
	}
	return changed
}

func monthlyLedgerTotals(entry *ApiKeyEntry, month string) (tokens int64, credits float64, requests int64) {
	for _, day := range entry.DailyUsage {
		if strings.HasPrefix(day.Date, month+"-") {
			tokens += day.TotalTokens
			credits += day.Credits
			requests += day.Requests
		}
	}
	return
}

const (
	// unknownModelName is the bucket for requests that arrive without a usable
	// model label, and the overflow bucket once maxModelsPerDay is reached.
	unknownModelName = "unknown"
	// maxModelNameLength bounds a single stored model label.
	maxModelNameLength = 128
	// maxModelsPerDay bounds per-day model cardinality in the persisted ledger.
	maxModelsPerDay = 64
)

func cloneApiKeyEntry(entry ApiKeyEntry) ApiKeyEntry {
	cp := entry
	if entry.DailyUsage != nil {
		cp.DailyUsage = append([]ApiKeyDailyUsage(nil), entry.DailyUsage...)
		for i := range cp.DailyUsage {
			if cp.DailyUsage[i].Models != nil {
				cp.DailyUsage[i].Models = append([]ApiKeyModelUsage(nil), cp.DailyUsage[i].Models...)
			}
		}
	}
	return cp
}

// ListApiKeys returns a snapshot of all configured API key entries.
func ListApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	for i := range cfg.ApiKeys {
		out[i] = cloneApiKeyEntry(cfg.ApiKeys[i])
	}
	return out
}

// GetApiKeyEntry returns a copy of the entry with the given ID, or nil if not found.
func GetApiKeyEntry(id string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cp := cloneApiKeyEntry(cfg.ApiKeys[i])
			return &cp
		}
	}
	return nil
}

// AddApiKey appends a new API key entry. Generates ID and CreatedAt if missing,
// rejects empty Key values, and refuses duplicates of an existing Key.
func AddApiKey(entry ApiKeyEntry) (ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	entry.Key = strings.TrimSpace(entry.Key)
	if entry.Key == "" {
		return ApiKeyEntry{}, errors.New("api key value must not be empty")
	}
	for _, existing := range cfg.ApiKeys {
		if existing.Key == entry.Key {
			return ApiKeyEntry{}, errors.New("api key already exists")
		}
	}
	if entry.ID == "" {
		entry.ID = newUUID()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	if entry.UsagePeriod == "" {
		entry.UsagePeriod = time.Now().UTC().Format(usagePeriodFormat)
	}
	entry = cloneApiKeyEntry(entry)
	cfg.ApiKeys = append(cfg.ApiKeys, entry)
	if err := saveLocked(); err != nil {
		// Roll back the in-memory append so we don't leave inconsistent state.
		cfg.ApiKeys = cfg.ApiKeys[:len(cfg.ApiKeys)-1]
		return ApiKeyEntry{}, err
	}
	return cloneApiKeyEntry(entry), nil
}

// UpdateApiKey applies a patch to an existing API key. Patch semantics:
//   - Name, Key are overwritten when non-empty in patch.
//   - Enabled, TokenLimit, CreditLimit are always overwritten (zero values are valid).
//   - Counters (TokensUsed/CreditsUsed/RequestsCount) are not touched here; use
//     RecordApiKeyUsage or ResetApiKeyUsage instead.
//   - Migrated stays as-is once true; only flips when explicitly set in patch.
func UpdateApiKey(id string, patch ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("api key not found")
	}
	if patch.Name != "" {
		cfg.ApiKeys[idx].Name = patch.Name
	}
	if patch.Key != "" {
		newKey := strings.TrimSpace(patch.Key)
		// Reject duplicates against any other entry.
		for j := range cfg.ApiKeys {
			if j != idx && cfg.ApiKeys[j].Key == newKey {
				return errors.New("api key value collides with existing entry")
			}
		}
		cfg.ApiKeys[idx].Key = newKey
	}
	cfg.ApiKeys[idx].Enabled = patch.Enabled
	cfg.ApiKeys[idx].TokenLimit = patch.TokenLimit
	cfg.ApiKeys[idx].CreditLimit = patch.CreditLimit
	if patch.Migrated {
		cfg.ApiKeys[idx].Migrated = true
	}
	return saveLocked()
}

// DeleteApiKey removes the API key entry with the given ID. Returns nil even if
// the ID is unknown (idempotent), matching the existing DeleteAccount style.
func DeleteApiKey(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i, e := range cfg.ApiKeys {
		if e.ID == id {
			cfg.ApiKeys = append(cfg.ApiKeys[:i], cfg.ApiKeys[i+1:]...)
			return saveLocked()
		}
	}
	return nil
}

// FindApiKeyByValue returns a copy of the entry whose Key matches the given value,
// or nil if no match. O(n) linear scan.
func FindApiKeyByValue(key string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || key == "" {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].Key == key {
			cp := cloneApiKeyEntry(cfg.ApiKeys[i])
			return &cp
		}
	}
	return nil
}

// HasApiKeys returns true when at least one API key entry is configured.
func HasApiKeys() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return len(cfg.ApiKeys) > 0
}

// RecordApiKeyUsage atomically adds input/output tokens and credits to
// the entry's cumulative counters and UTC daily ledger, then persists the update.
// The model argument attributes the request to a model name inside the daily
// record; an empty model is stored under "unknown".
func RecordApiKeyUsage(id string, inputTokens, outputTokens int64, credits float64) error {
	return recordApiKeyUsageAt(id, "", inputTokens, outputTokens, credits, time.Now())
}

// RecordApiKeyModelUsage is RecordApiKeyUsage with model attribution.
func RecordApiKeyModelUsage(id, model string, inputTokens, outputTokens int64, credits float64) error {
	return recordApiKeyUsageAt(id, model, inputTokens, outputTokens, credits, time.Now())
}

func recordApiKeyUsageAt(id, model string, inputTokens, outputTokens int64, credits float64, now time.Time) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	resetChanged := ensureApiKeyMonthlyResetLocked(now)
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			if inputTokens < 0 {
				inputTokens = 0
			}
			if outputTokens < 0 {
				outputTokens = 0
			}
			if credits < 0 {
				credits = 0
			}
			totalTokens := inputTokens + outputTokens
			if totalTokens > 0 {
				cfg.ApiKeys[i].TokensUsed += totalTokens
			}
			if credits != 0 {
				cfg.ApiKeys[i].CreditsUsed += credits
			}
			cfg.ApiKeys[i].RequestsCount++
			cfg.ApiKeys[i].LastUsedAt = now.Unix()
			date := now.UTC().Format("2006-01-02")
			appendDailyUsage(&cfg.ApiKeys[i], date, normalizeModelName(model), inputTokens, outputTokens, totalTokens, credits)
			return saveLocked()
		}
	}
	if resetChanged {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return errors.New("api key not found")
}

// normalizeModelName trims and lower-bounds a model label so the ledger never
// stores empty keys and stays stable across casing differences from clients.
func normalizeModelName(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return unknownModelName
	}
	if len(model) > maxModelNameLength {
		model = model[:maxModelNameLength]
	}
	return model
}

func appendDailyUsage(entry *ApiKeyEntry, date, model string, inputTokens, outputTokens, totalTokens int64, credits float64) {
	idx := sort.Search(len(entry.DailyUsage), func(i int) bool {
		return entry.DailyUsage[i].Date >= date
	})
	if idx < len(entry.DailyUsage) && entry.DailyUsage[idx].Date == date {
		usage := &entry.DailyUsage[idx]
		usage.Requests++
		usage.InputTokens += inputTokens
		usage.OutputTokens += outputTokens
		usage.TotalTokens += totalTokens
		usage.Credits += credits
		addModelUsage(usage, model, inputTokens, outputTokens, totalTokens, credits)
		return
	}
	usage := ApiKeyDailyUsage{
		Date:         date,
		Requests:     1,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
		Credits:      credits,
	}
	addModelUsage(&usage, model, inputTokens, outputTokens, totalTokens, credits)
	entry.DailyUsage = append(entry.DailyUsage, ApiKeyDailyUsage{})
	copy(entry.DailyUsage[idx+1:], entry.DailyUsage[idx:])
	entry.DailyUsage[idx] = usage
}

// addModelUsage merges one request into the day's per-model breakdown, keeping
// the slice sorted by model name so persisted output is deterministic.
func addModelUsage(usage *ApiKeyDailyUsage, model string, inputTokens, outputTokens, totalTokens int64, credits float64) {
	idx := sort.Search(len(usage.Models), func(i int) bool {
		return usage.Models[i].Model >= model
	})
	if idx < len(usage.Models) && usage.Models[idx].Model == model {
		row := &usage.Models[idx]
		row.Requests++
		row.InputTokens += inputTokens
		row.OutputTokens += outputTokens
		row.TotalTokens += totalTokens
		row.Credits += credits
		return
	}
	if len(usage.Models) >= maxModelsPerDay {
		// Cap cardinality so a client sending random model names cannot grow
		// the config file without bound; overflow lands in the unknown bucket.
		if model != unknownModelName {
			addModelUsage(usage, unknownModelName, inputTokens, outputTokens, totalTokens, credits)
		}
		return
	}
	row := ApiKeyModelUsage{
		Model:        model,
		Requests:     1,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
		Credits:      credits,
	}
	usage.Models = append(usage.Models, ApiKeyModelUsage{})
	copy(usage.Models[idx+1:], usage.Models[idx:])
	usage.Models[idx] = row
}

// ResetApiKeyUsage clears cumulative quota counters and records the reset time.
// LastUsedAt and the permanent daily ledger are preserved.
func ResetApiKeyUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	now := time.Now()
	resetChanged := ensureApiKeyMonthlyResetLocked(now)
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			cfg.ApiKeys[i].UsageResetAt = usageMonthStart(now).Unix()
			cfg.ApiKeys[i].UsagePeriod = usageMonth(now)
			return saveLocked()
		}
	}
	if resetChanged {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return errors.New("api key not found")
}

// GenerateApiKeyValue returns a new random 32-byte hex API key prefixed with "sk-".
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey produces a display-friendly masked version without ever returning
// a non-empty key verbatim.
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return strings.Repeat("*", len(key))
	}
	if len(key) <= 10 {
		return key[:2] + "****" + key[len(key)-2:]
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// ApiKeyOverLimit returns (overToken, overCredit) for the entry. Limits with value 0
// are ignored. The function does not lock; callers should pass a copied entry.
func ApiKeyOverLimit(e ApiKeyEntry) (overToken bool, overCredit bool) {
	if e.TokenLimit > 0 && e.TokensUsed >= e.TokenLimit {
		overToken = true
	}
	if e.CreditLimit > 0 && e.CreditsUsed >= e.CreditLimit {
		overCredit = true
	}
	return
}
