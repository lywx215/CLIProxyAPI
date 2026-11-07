package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type enterpriseBridgeLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *enterpriseBridgeLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *enterpriseBridgeLog) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

// This optional local integration test invokes the companion repository's actual
// channel HTTP construction path. Both upstream services are loopback fixtures.
func TestEnterpriseNewAPIRequestIDBridge(t *testing.T) {
	newAPI := os.Getenv("NEWAPI_CORRELATION_REPO")
	if newAPI == "" {
		t.Skip("set NEWAPI_CORRELATION_REPO to the companion local checkout")
	}
	if _, err := os.Stat(filepath.Join(newAPI, "relay", "channel", "request_correlation_test.go")); err != nil {
		t.Fatal("companion correlation tests are missing")
	}
	logger := log.StandardLogger()
	oldOutput, oldLevel, oldFormatter := logger.Out, logger.GetLevel(), logger.Formatter
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	t.Cleanup(func() {
		logger.SetOutput(oldOutput)
		logger.SetLevel(oldLevel)
		logger.SetFormatter(oldFormatter)
		logger.ReplaceHooks(oldHooks)
	})
	buffer := &enterpriseBridgeLog{}
	logger.SetOutput(buffer)
	logger.SetLevel(log.WarnLevel)
	logger.SetFormatter(&logging.LogFormatter{})
	var googleCalls atomic.Int32
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		googleCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "query.text").String() != "最终 payload query" || gjson.GetBytes(body, "generationSpec.modelId").String() != "gemini-3.8-flash" {
			t.Error("diagnostics changed the final business payload")
		}
		if r.Header.Get("X-Request-Id") != "" || r.Header.Get("X-Diag-Request-Id") != "" {
			t.Error("caller identities were automatically forwarded to Google")
		}
		w.Header().Set("X-Goog-Request-Id", "11111111-2222-3333-4444-555555555555")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("outcome") {
		case "http-error":
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"query.text must be a string: private-value"}}`)
		case "stream-error":
			_, _ = io.WriteString(w, `[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"partial"}}]}},{"error":{"code":3,"status":"INVALID_ARGUMENT","message":"query.text must be a string: private-value"}}]`)
		default:
			_, _ = io.WriteString(w, enterpriseAnswer)
		}
	}))
	t.Cleanup(google.Close)
	googleURL, _ := url.Parse(google.URL)
	capture := &enterpriseUsageCapture{records: make(chan usage.Record, 128)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
	auth := enterpriseExecutorAuth(t, enterpriseExecutorIdentity("enterprise-usage-capture", "us"))
	auth.Metadata["enterprise_collection_revision"] = uint64(9)
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini-enterprise"}}, Params: map[string]any{"query.text": "最终 payload query"}}}}}
	executor := NewGeminiEnterpriseExecutor(cfg)
	d := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "[]", func() bool { return true }, func() bool { return false }, func([]byte) error { return nil })
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Writer = &enterpriseBridgeHeaders{ResponseWriter: c.Writer, outcome: c.Query("outcome")}
		c.Next()
	}, logging.GinLogrusLogger(), d.GinMiddleware(logging.GetGinRequestID))
	engine.POST("/*path", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		format := translator.FormatOpenAI
		stream := gjson.GetBytes(body, "stream").Bool()
		if strings.HasPrefix(c.Request.URL.Path, "/v1beta/") {
			format, stream = translator.FormatGemini, strings.HasSuffix(c.Request.URL.Path, ":streamGenerateContent")
		}
		outcome := c.Query("outcome")
		transport := enterpriseTransport(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			u := *r.URL
			u.Scheme, u.Host, u.RawQuery = googleURL.Scheme, googleURL.Host, "outcome="+outcome
			clone.URL, clone.Host = &u, googleURL.Host
			return http.DefaultTransport.RoundTrip(clone)
		})
		ctx := context.WithValue(c.Request.Context(), "cliproxy.roundtripper", transport)
		ctx = context.WithValue(ctx, "gin", c)
		req := coreexec.Request{Model: "gemini-3.8-flash", Payload: body}
		opts := enterpriseOptions(format)
		if !stream {
			result, err := executor.Execute(ctx, auth, req, opts)
			if err != nil {
				c.Data(400, "application/json", handlers.BuildErrorResponseBodyWithError(400, err.Error(), err))
				return
			}
			c.Data(200, "application/json", result.Payload)
			return
		}
		result, err := executor.ExecuteStream(ctx, auth, req, opts)
		if err != nil {
			c.Data(400, "application/json", handlers.BuildErrorResponseBodyWithError(400, err.Error(), err))
			return
		}
		c.Header("Content-Type", "text/event-stream")
		failed := false
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				failed = true
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", handlers.BuildErrorResponseBodyWithError(400, chunk.Err.Error(), chunk.Err))
			} else {
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", chunk.Payload)
			}
		}
		if !failed && format == translator.FormatOpenAI {
			_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
		}
	})
	cpa := httptest.NewServer(engine)
	t.Cleanup(cpa.Close)
	command := exec.CommandContext(t.Context(), "go", "test", "./relay/channel", "-run", "^TestCLIProxyAPIRequestIDBridge$", "-count=1")
	command.Dir = newAPI
	command.Env = append(os.Environ(), "CPA_DIAGNOSTIC_BRIDGE_URL="+cpa.URL)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("companion bridge failed: %v\n%s", err, output)
	}
	if googleCalls.Load() != 40 {
		t.Fatalf("unexpected upstream replay or missing request: %d", googleCalls.Load())
	}
	failures, knownInput := 0, 0
	for range 40 {
		select {
		case record := <-capture.records:
			if record.Failed {
				failures++
			}
			if record.Detail.InputTokens > 0 {
				knownInput++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing usage settlement")
		}
	}
	if failures != 16 || knownInput != 32 {
		t.Fatalf("existing accounting changed: failures=%d known_input=%d", failures, knownInput)
	}
	text := buffer.text()
	if strings.Contains(text, "private-") || strings.Contains(text, "最终 payload query") || strings.Count(text, "Enterprise request failed") != 16 {
		t.Fatal("safe failure logs leaked values or lost/duplicated executions")
	}
	for _, required := range []string{`caller_request_id="newapi-`, `diagnostic_request_id="`, `upstream_request_id="11111111-`, `reason_class="invalid_type"`, `query_chars=`, `binding_revision=9`} {
		if !strings.Contains(text, required) {
			t.Fatalf("integration logs omitted %s", required)
		}
	}
	t.Logf("40 requests via actual New API HTTP construction, 16 failures, 40 single usage settlements; companion: %s", strings.TrimSpace(string(output)))
}

// Rewrite fixture-only peer headers at the final writer boundary, after the
// real diagnostics middleware has supplied its local request ID.
type enterpriseBridgeHeaders struct {
	gin.ResponseWriter
	outcome string
}

func (w *enterpriseBridgeHeaders) rewrite() {
	switch w.outcome {
	case "invalid-id":
		w.Header().Set("X-Diag-Request-Id", "invalid id")
	case "missing-id":
		w.Header().Del("X-Diag-Request-Id")
	}
}

func (w *enterpriseBridgeHeaders) Write(p []byte) (int, error) {
	w.rewrite()
	return w.ResponseWriter.Write(p)
}

func (w *enterpriseBridgeHeaders) WriteHeader(status int) {
	w.rewrite()
	w.ResponseWriter.WriteHeader(status)
}

func (w *enterpriseBridgeHeaders) WriteHeaderNow() {
	w.rewrite()
	w.ResponseWriter.WriteHeaderNow()
}
