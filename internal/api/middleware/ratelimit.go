package middleware

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const rateLimitWindow = time.Minute // sliding window duration

// RateLimitConfig holds the per-API-key rate limiting configuration.
type RateLimitConfig struct {
	// DefaultRPM is the default requests-per-minute for keys not in Overrides.
	// 0 means no rate limiting.
	DefaultRPM int

	// Overrides maps specific API keys to custom RPM limits.
	Overrides map[string]int
}

// APIKeyRateLimiter provides per-API-key sliding-window rate limiting.
type APIKeyRateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time       // apiKey -> request timestamps
	config   atomic.Pointer[RateLimitConfig] // supports hot-reload
}

// NewAPIKeyRateLimiter creates a new rate limiter with the given initial configuration.
func NewAPIKeyRateLimiter(cfg RateLimitConfig) *APIKeyRateLimiter {
	rl := &APIKeyRateLimiter{
		requests: make(map[string][]time.Time),
	}
	rl.config.Store(&cfg)
	return rl
}

// UpdateConfig atomically replaces the rate limit configuration.
// Existing request history is preserved so that in-flight windows remain accurate.
func (rl *APIKeyRateLimiter) UpdateConfig(cfg RateLimitConfig) {
	rl.config.Store(&cfg)
	log.Debugf("api-key rate limit config updated: default-rpm=%d, overrides=%d", cfg.DefaultRPM, len(cfg.Overrides))
}

// rpmForKey returns the effective RPM limit for the given API key.
// Returns 0 if no rate limiting should be applied.
func (rl *APIKeyRateLimiter) rpmForKey(apiKey string) int {
	cfg := rl.config.Load()
	if cfg == nil {
		return 0
	}
	if rpm, ok := cfg.Overrides[apiKey]; ok {
		return rpm
	}
	return cfg.DefaultRPM
}

// Allow checks whether a request from the given API key is allowed.
// Returns true and zero duration if allowed.
// Returns false and the retry-after duration if rate limited.
func (rl *APIKeyRateLimiter) Allow(apiKey string) (bool, time.Duration) {
	if apiKey == "" {
		return true, 0
	}

	rpm := rl.rpmForKey(apiKey)
	if rpm <= 0 {
		return true, 0
	}

	now := time.Now()
	windowStart := now.Add(-rateLimitWindow)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Get and filter timestamps within the window
	timestamps := rl.requests[apiKey]
	var valid []time.Time
	for _, ts := range timestamps {
		if ts.After(windowStart) {
			valid = append(valid, ts)
		}
	}

	// Prune expired entries to prevent memory leak
	if len(valid) == 0 {
		delete(rl.requests, apiKey)
	}

	// Check if rate limit exceeded
	if len(valid) >= rpm {
		// Calculate when the oldest request in the window will expire
		oldest := valid[0]
		retryAfter := oldest.Add(rateLimitWindow).Sub(now)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return false, retryAfter
	}

	// Record this request
	valid = append(valid, now)
	rl.requests[apiKey] = valid

	return true, 0
}

// RateLimitMiddleware creates a Gin middleware that enforces per-API-key rate limiting.
// It reads the authenticated API key from the Gin context (set by AuthMiddleware as "apiKey").
// When rate limited, it responds with 429 Too Many Requests and a Retry-After header.
func RateLimitMiddleware(limiter *APIKeyRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limiter == nil {
			c.Next()
			return
		}

		apiKeyVal, exists := c.Get("apiKey")
		if !exists {
			// No API key in context (unauthenticated or auth disabled) — skip rate limiting
			c.Next()
			return
		}

		apiKey, ok := apiKeyVal.(string)
		if !ok || apiKey == "" {
			c.Next()
			return
		}

		allowed, retryAfter := limiter.Allow(apiKey)
		if allowed {
			c.Next()
			return
		}

		retryAfterSec := int(retryAfter.Seconds())
		if retryAfterSec < 1 {
			retryAfterSec = 1
		}

		log.Debugf("api-key rate limit exceeded for key %s...%s, retry after %ds",
			redactKey(apiKey), "", retryAfterSec)

		c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error": gin.H{
				"code":    "rate_limit_exceeded",
				"message": fmt.Sprintf("API key rate limit exceeded. Please retry after %d seconds.", retryAfterSec),
				"type":    "rate_limit_exceeded",
			},
		})
	}
}

// redactKey returns a redacted version of the API key for safe logging.
func redactKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}
