package auth

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// RequestStatEntry records request statistics for a single auth×model combination.
type RequestStatEntry struct {
	AuthID       string        `json:"auth_id"`
	Model        string        `json:"model"`
	TotalCount   int64         `json:"total"`
	SuccessCount int64         `json:"success"`
	FailureCount int64         `json:"failure"`
	CreditsCount int64         `json:"credits_count"`
	StatusCodes  map[int]int64 `json:"status_codes,omitempty"`
	FirstSeen    time.Time     `json:"first_seen"`
	LastSeen     time.Time     `json:"last_seen"`
}

// AuthStatSummary aggregates statistics by auth (across all models).
type AuthStatSummary struct {
	AuthID       string        `json:"auth_id"`
	TotalCount   int64         `json:"total"`
	SuccessCount int64         `json:"success"`
	FailureCount int64         `json:"failure"`
	CreditsCount int64         `json:"credits_count"`
	ErrorRate    string        `json:"error_rate"`
	StatusCodes  map[int]int64 `json:"status_codes,omitempty"`
	Models       []string      `json:"models"`
	FirstSeen    time.Time     `json:"first_seen"`
	LastSeen     time.Time     `json:"last_seen"`
}

// ModelStatSummary aggregates statistics by model (across all auths).
type ModelStatSummary struct {
	Model        string        `json:"model"`
	TotalCount   int64         `json:"total"`
	SuccessCount int64         `json:"success"`
	FailureCount int64         `json:"failure"`
	CreditsCount int64         `json:"credits_count"`
	ErrorRate    string        `json:"error_rate"`
	StatusCodes  map[int]int64 `json:"status_codes,omitempty"`
	AuthCount    int           `json:"auth_count"`
	FirstSeen    time.Time     `json:"first_seen"`
	LastSeen     time.Time     `json:"last_seen"`
}

// StatsSummary provides an overall summary of all tracked statistics.
type StatsSummary struct {
	TotalAuths    int    `json:"total_auths"`
	TotalModels   int    `json:"total_models"`
	TotalRequests int64  `json:"total_requests"`
	TotalSuccess  int64  `json:"total_success"`
	TotalFailure  int64  `json:"total_failure"`
	TotalCredits  int64  `json:"total_credits"`
	ErrorRate     string `json:"error_rate"`
}

// RequestStatsTracker tracks per-auth per-model request statistics for the Antigravity provider.
type RequestStatsTracker struct {
	mu    sync.RWMutex
	stats map[string]*RequestStatEntry // key: "authID|model"
}

// globalRequestStats is a package-level singleton tracker for Antigravity request statistics.
var globalRequestStats = &RequestStatsTracker{
	stats: make(map[string]*RequestStatEntry),
}

func requestStatsKey(authID, model string) string {
	return authID + "|" + model
}

// RecordAntigravityResult records a request result if the provider is "antigravity".
// This function is safe for concurrent use.
func RecordAntigravityResult(result Result) {
	provider := strings.ToLower(strings.TrimSpace(result.Provider))
	if provider != "antigravity" {
		return
	}
	authID := strings.TrimSpace(result.AuthID)
	if authID == "" {
		return
	}
	model := strings.TrimSpace(result.Model)
	if model == "" {
		model = "(unknown)"
	}

	statusCode := 0
	if !result.Success && result.Error != nil {
		statusCode = result.Error.HTTPStatus
	}

	globalRequestStats.record(authID, model, result.Success, statusCode, result.CreditsUsed)
}

func (t *RequestStatsTracker) record(authID, model string, success bool, statusCode int, creditsUsed bool) {
	now := time.Now()
	key := requestStatsKey(authID, model)

	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.stats[key]
	if !ok {
		entry = &RequestStatEntry{
			AuthID:      authID,
			Model:       model,
			StatusCodes: make(map[int]int64),
			FirstSeen:   now,
		}
		t.stats[key] = entry
	}

	entry.TotalCount++
	entry.LastSeen = now

	if success {
		entry.SuccessCount++
	} else {
		entry.FailureCount++
		if statusCode > 0 {
			entry.StatusCodes[statusCode]++
		}
	}
	if creditsUsed {
		entry.CreditsCount++
	}
}

// GetAllStats returns a snapshot of all auth×model entries.
func GetAllStats() []*RequestStatEntry {
	return globalRequestStats.getAllStats()
}

func (t *RequestStatsTracker) getAllStats() []*RequestStatEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]*RequestStatEntry, 0, len(t.stats))
	for _, entry := range t.stats {
		out = append(out, cloneStatEntry(entry))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AuthID != out[j].AuthID {
			return out[i].AuthID < out[j].AuthID
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// GetStatsByAuth returns all entries for a specific authID.
func GetStatsByAuth(authID string) []*RequestStatEntry {
	return globalRequestStats.getStatsByAuth(authID)
}

func (t *RequestStatsTracker) getStatsByAuth(authID string) []*RequestStatEntry {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	var out []*RequestStatEntry
	for _, entry := range t.stats {
		if entry.AuthID == authID {
			out = append(out, cloneStatEntry(entry))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Model < out[j].Model
	})
	return out
}

// GetAuthSummary returns statistics aggregated by auth (combining all models).
func GetAuthSummary() []AuthStatSummary {
	return globalRequestStats.getAuthSummary()
}

func (t *RequestStatsTracker) getAuthSummary() []AuthStatSummary {
	t.mu.RLock()
	defer t.mu.RUnlock()

	byAuth := make(map[string]*AuthStatSummary)
	for _, entry := range t.stats {
		summary, ok := byAuth[entry.AuthID]
		if !ok {
			summary = &AuthStatSummary{
				AuthID:      entry.AuthID,
				StatusCodes: make(map[int]int64),
				FirstSeen:   entry.FirstSeen,
				LastSeen:    entry.LastSeen,
			}
			byAuth[entry.AuthID] = summary
		}
		summary.TotalCount += entry.TotalCount
		summary.SuccessCount += entry.SuccessCount
		summary.FailureCount += entry.FailureCount
		summary.CreditsCount += entry.CreditsCount
		summary.Models = append(summary.Models, entry.Model)
		for code, count := range entry.StatusCodes {
			summary.StatusCodes[code] += count
		}
		if entry.FirstSeen.Before(summary.FirstSeen) {
			summary.FirstSeen = entry.FirstSeen
		}
		if entry.LastSeen.After(summary.LastSeen) {
			summary.LastSeen = entry.LastSeen
		}
	}

	out := make([]AuthStatSummary, 0, len(byAuth))
	for _, summary := range byAuth {
		summary.ErrorRate = formatErrorRate(summary.FailureCount, summary.TotalCount)
		sort.Strings(summary.Models)
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].TotalCount > out[j].TotalCount
	})
	return out
}

// GetModelSummary returns statistics aggregated by model (combining all auths).
func GetModelSummary() []ModelStatSummary {
	return globalRequestStats.getModelSummary()
}

func (t *RequestStatsTracker) getModelSummary() []ModelStatSummary {
	t.mu.RLock()
	defer t.mu.RUnlock()

	type modelAcc struct {
		summary ModelStatSummary
		auths   map[string]struct{}
	}
	byModel := make(map[string]*modelAcc)
	for _, entry := range t.stats {
		acc, ok := byModel[entry.Model]
		if !ok {
			acc = &modelAcc{
				summary: ModelStatSummary{
					Model:       entry.Model,
					StatusCodes: make(map[int]int64),
					FirstSeen:   entry.FirstSeen,
					LastSeen:    entry.LastSeen,
				},
				auths: make(map[string]struct{}),
			}
			byModel[entry.Model] = acc
		}
		acc.summary.TotalCount += entry.TotalCount
		acc.summary.SuccessCount += entry.SuccessCount
		acc.summary.FailureCount += entry.FailureCount
		acc.summary.CreditsCount += entry.CreditsCount
		acc.auths[entry.AuthID] = struct{}{}
		for code, count := range entry.StatusCodes {
			acc.summary.StatusCodes[code] += count
		}
		if entry.FirstSeen.Before(acc.summary.FirstSeen) {
			acc.summary.FirstSeen = entry.FirstSeen
		}
		if entry.LastSeen.After(acc.summary.LastSeen) {
			acc.summary.LastSeen = entry.LastSeen
		}
	}

	out := make([]ModelStatSummary, 0, len(byModel))
	for _, acc := range byModel {
		acc.summary.AuthCount = len(acc.auths)
		acc.summary.ErrorRate = formatErrorRate(acc.summary.FailureCount, acc.summary.TotalCount)
		out = append(out, acc.summary)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].TotalCount > out[j].TotalCount
	})
	return out
}

// GetStatsSummary returns an overall summary across all tracked entries.
func GetStatsSummary() StatsSummary {
	return globalRequestStats.getStatsSummary()
}

func (t *RequestStatsTracker) getStatsSummary() StatsSummary {
	t.mu.RLock()
	defer t.mu.RUnlock()

	auths := make(map[string]struct{})
	models := make(map[string]struct{})
	var total, success, failure, credits int64

	for _, entry := range t.stats {
		auths[entry.AuthID] = struct{}{}
		models[entry.Model] = struct{}{}
		total += entry.TotalCount
		success += entry.SuccessCount
		failure += entry.FailureCount
		credits += entry.CreditsCount
	}

	return StatsSummary{
		TotalAuths:    len(auths),
		TotalModels:   len(models),
		TotalRequests: total,
		TotalSuccess:  success,
		TotalFailure:  failure,
		TotalCredits:  credits,
		ErrorRate:     formatErrorRate(failure, total),
	}
}

// ResetAllStats clears all tracked statistics. Returns number of entries cleared.
func ResetAllStats() int {
	return globalRequestStats.resetAll()
}

func (t *RequestStatsTracker) resetAll() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := len(t.stats)
	t.stats = make(map[string]*RequestStatEntry)
	return count
}

// ResetStatsByAuth clears statistics for a specific authID. Returns number of entries cleared.
func ResetStatsByAuth(authID string) int {
	return globalRequestStats.resetByAuth(authID)
}

func (t *RequestStatsTracker) resetByAuth(authID string) int {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	count := 0
	for key, entry := range t.stats {
		if entry.AuthID == authID {
			delete(t.stats, key)
			count++
		}
	}
	return count
}

func cloneStatEntry(e *RequestStatEntry) *RequestStatEntry {
	if e == nil {
		return nil
	}
	clone := *e
	if len(e.StatusCodes) > 0 {
		clone.StatusCodes = make(map[int]int64, len(e.StatusCodes))
		for k, v := range e.StatusCodes {
			clone.StatusCodes[k] = v
		}
	}
	return &clone
}

func formatErrorRate(failures, total int64) string {
	if total <= 0 {
		return "0.0%"
	}
	rate := float64(failures) / float64(total) * 100
	return fmt.Sprintf("%.1f%%", rate)
}
