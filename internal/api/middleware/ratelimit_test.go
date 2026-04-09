package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestAPIKeyRateLimiter_AllowWithinLimit(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 5})
	for i := 0; i < 5; i++ {
		allowed, _ := rl.Allow("key-a")
		if !allowed {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
}

func TestAPIKeyRateLimiter_DenyOverLimit(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 3})
	for i := 0; i < 3; i++ {
		allowed, _ := rl.Allow("key-a")
		if !allowed {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	allowed, retryAfter := rl.Allow("key-a")
	if allowed {
		t.Fatal("4th request should be denied")
	}
	if retryAfter < time.Second {
		t.Fatalf("retryAfter should be >= 1s, got %v", retryAfter)
	}
}

func TestAPIKeyRateLimiter_IndependentKeys(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 2})
	// Exhaust key-a
	rl.Allow("key-a")
	rl.Allow("key-a")
	allowed, _ := rl.Allow("key-a")
	if allowed {
		t.Fatal("key-a should be rate limited")
	}
	// key-b should still be allowed
	allowed, _ = rl.Allow("key-b")
	if !allowed {
		t.Fatal("key-b should be allowed (independent from key-a)")
	}
}

func TestAPIKeyRateLimiter_OverrideRPM(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{
		DefaultRPM: 2,
		Overrides: map[string]int{
			"vip-key": 5,
		},
	})
	// Default key gets limited at 2
	rl.Allow("normal-key")
	rl.Allow("normal-key")
	allowed, _ := rl.Allow("normal-key")
	if allowed {
		t.Fatal("normal-key should be rate limited at 2 RPM")
	}
	// VIP key can do 5
	for i := 0; i < 5; i++ {
		allowed, _ := rl.Allow("vip-key")
		if !allowed {
			t.Fatalf("vip-key request %d should be allowed (limit=5)", i+1)
		}
	}
	allowed, _ = rl.Allow("vip-key")
	if allowed {
		t.Fatal("vip-key should be rate limited at 5 RPM")
	}
}

func TestAPIKeyRateLimiter_ZeroRPMNoLimit(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 0})
	for i := 0; i < 100; i++ {
		allowed, _ := rl.Allow("any-key")
		if !allowed {
			t.Fatalf("request %d should be allowed when RPM=0 (no limit)", i+1)
		}
	}
}

func TestAPIKeyRateLimiter_EmptyKeySkipped(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 1})
	for i := 0; i < 10; i++ {
		allowed, _ := rl.Allow("")
		if !allowed {
			t.Fatal("empty key should always be allowed")
		}
	}
}

func TestAPIKeyRateLimiter_UpdateConfig(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 2})
	rl.Allow("key-a")
	rl.Allow("key-a")
	allowed, _ := rl.Allow("key-a")
	if allowed {
		t.Fatal("should be rate limited at 2 RPM")
	}
	// Update config to allow more
	rl.UpdateConfig(RateLimitConfig{DefaultRPM: 10})
	allowed, _ = rl.Allow("key-a")
	if !allowed {
		t.Fatal("should be allowed after config update to 10 RPM")
	}
}

func TestRateLimitMiddleware_AllowsRequest(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 10})
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("apiKey", "test-key")
		c.Next()
	})
	engine.Use(RateLimitMiddleware(rl))
	engine.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestRateLimitMiddleware_Returns429(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 1})
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("apiKey", "test-key")
		c.Next()
	})
	engine.Use(RateLimitMiddleware(rl))
	engine.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// First request should pass
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w.Code)
	}

	// Second request should be rate limited
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d", w.Code)
	}

	// Check Retry-After header
	retryAfter := w.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("expected Retry-After header")
	}

	// Check error response body
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object in response body")
	}
	if errObj["code"] != "rate_limit_exceeded" {
		t.Fatalf("expected code=rate_limit_exceeded, got %v", errObj["code"])
	}
}

func TestRateLimitMiddleware_NoApiKey(t *testing.T) {
	rl := NewAPIKeyRateLimiter(RateLimitConfig{DefaultRPM: 1})
	engine := gin.New()
	// Intentionally no apiKey set in context
	engine.Use(RateLimitMiddleware(rl))
	engine.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Should always pass regardless of RPM since no apiKey in context
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d without apiKey: expected 200, got %d", i+1, w.Code)
		}
	}
}

func TestRateLimitMiddleware_NilLimiter(t *testing.T) {
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("apiKey", "test-key")
		c.Next()
	})
	engine.Use(RateLimitMiddleware(nil))
	engine.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("nil limiter: expected 200, got %d", w.Code)
	}
}

func TestRedactKey(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "***"},
		{"short", "***"},
		{"12345678", "***"},
		{"123456789", "1234...6789"},
		{"gemini123!!!", "gemi...3!!!"},
	}
	for _, tt := range tests {
		got := redactKey(tt.input)
		if got != tt.expected {
			t.Errorf("redactKey(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
