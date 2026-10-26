package geminienterprise

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// SelectQuotaPool never substitutes another edition for a verified assignment.
// A single active pool is an admissible fallback only when identity is unknown.
func SelectQuotaPool(snapshot QuotaSnapshot, identity *QuotaIdentity, binding config.GeminiEnterpriseOAuth) *QuotaPool {
	if identity != nil && identity.Invalidated {
		return nil
	}
	if identity.ValidFor(binding) {
		for _, pool := range snapshot.Pools {
			if pool.Tier == identity.Edition {
				return cloneQuotaPool(pool)
			}
		}
		return nil
	}
	if len(snapshot.Pools) == 1 {
		return cloneQuotaPool(snapshot.Pools[0])
	}
	return nil
}

// MergeQuotaObservations keeps independent successful readings on transient
// failures, but never carries a balance across a Pacific daily reset.
func MergeQuotaObservations(previous, current QuotaSnapshot) QuotaSnapshot {
	result := CloneQuotaSnapshot(current)
	if previous.ProjectID != current.ProjectID || previous.Location != current.Location || previous.PeriodStart == nil || current.PeriodStart == nil || !previous.PeriodStart.Equal(*current.PeriodStart) {
		return result
	}
	if previous.ObservedAt.After(current.ObservedAt) {
		return CloneQuotaSnapshot(previous)
	}
	if len(current.Pools) == 0 && (current.Status == "unavailable" || current.Status == "no_permission") {
		result.Pools = CloneQuotaSnapshot(previous).Pools
		for i := range result.Pools {
			result.Pools[i].Status = "stale"
		}
		result.Status = quotaStatus(result.Pools, result.Status)
		result.ObservedAt = previous.ObservedAt
		return result
	}
	for i := range result.Pools {
		pool := &result.Pools[i]
		for _, old := range previous.Pools {
			if pool.Tier != old.Tier {
				continue
			}
			if pool.Version != "" && old.Version != "" && pool.Version != old.Version {
				break
			}
			// An empty successful observation may be a conflict or a new version.
			// Only transport/permission failures allow reusing earlier fields.
			if pool.Status == "partial" || pool.Status == "unavailable" || pool.Status == "no_permission" {
				prior := cloneQuotaPool(old)
				retained := false
				if pool.Limit == nil && prior.Limit != nil {
					pool.Limit, retained = prior.Limit, true
				}
				if pool.Used == nil && prior.Used != nil {
					pool.Used, pool.UsageUpdatedAt, retained = prior.Used, prior.UsageUpdatedAt, true
				}
				if retained {
					pool.Status = "stale"
				}
			}
			if pool.UsageUpdatedAt != nil && old.UsageUpdatedAt != nil && old.UsageUpdatedAt.After(*pool.UsageUpdatedAt) {
				prior := cloneQuotaPool(old)
				pool.Used, pool.UsageUpdatedAt = prior.Used, prior.UsageUpdatedAt
			}
			pool.Remaining = nil
			if pool.Limit != nil && pool.Used != nil {
				remaining := max(int64(0), *pool.Limit-*pool.Used)
				pool.Remaining = &remaining
			}
			break
		}
	}
	result.Status = quotaStatus(result.Pools, result.Status)
	return result
}

func CloneQuotaSnapshot(snapshot QuotaSnapshot) QuotaSnapshot {
	result := snapshot
	result.PeriodStart, result.ResetAt = cloneQuotaTime(snapshot.PeriodStart), cloneQuotaTime(snapshot.ResetAt)
	result.Pools = make([]QuotaPool, len(snapshot.Pools))
	for i, pool := range snapshot.Pools {
		result.Pools[i] = *cloneQuotaPool(pool)
	}
	return result
}

func cloneQuotaPool(pool QuotaPool) *QuotaPool {
	result := pool
	for _, pair := range []struct {
		source *int64
		target **int64
	}{
		{pool.Limit, &result.Limit}, {pool.Used, &result.Used}, {pool.Remaining, &result.Remaining},
	} {
		if pair.source != nil {
			value := *pair.source
			*pair.target = &value
		}
	}
	result.UsageUpdatedAt = cloneQuotaTime(pool.UsageUpdatedAt)
	return &result
}

func cloneQuotaTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
