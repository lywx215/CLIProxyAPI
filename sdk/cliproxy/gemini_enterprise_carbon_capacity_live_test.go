package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type carbonCapacityCaseKey struct{}

// TestEnterpriseCarbonCapacityHarness is an opt-in loopback-only live test host.
// Credentials stay in memory, and all control endpoints exist only in this test.
func TestEnterpriseCarbonCapacityHarness(t *testing.T) {
	t.Skip("Historical capacity validation is complete; use authenticated fixed-application Carbon management tests for new verification")
	raw := os.Getenv("CLIPROXY_ENTERPRISE_CARBON_CAPACITY_JSON")
	if raw == "" {
		t.Skip("requires explicit capacity-test authorization and an in-memory credential")
	}
	var input struct {
		Metadata    map[string]any `json:"metadata"`
		Ready       string         `json:"ready"`
		ForceAssist bool           `json:"force_assist"`
	}
	if json.Unmarshal([]byte(raw), &input) != nil || input.Ready == "" {
		t.Fatal("invalid live capacity input")
	}
	application := ge.FromMetadata(input.Metadata)
	if application.Validate() != nil || application.ProjectID != "1002653018111" || application.Location != "global" {
		t.Fatal("capacity test requires the explicitly authorized collection")
	}
	cfg := &config.Config{}
	if input.ForceAssist {
		cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: ge.CarbonModel, Protocol: ge.Provider}}, Params: map[string]any{"assistSkippingMode": "REQUEST_ASSIST"}}}
	}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	executor := runtimeexec.NewGeminiEnterpriseExecutor(cfg)
	manager.RegisterExecutor(executor)
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: "carbon-capacity-isolated", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: input.Metadata})
	if err != nil {
		t.Fatal("capacity credential registration failed")
	}
	svc := &Service{cfg: cfg, coreManager: manager, antigravityContext: t.Context()}
	t.Cleanup(func() {
		svc.cancelEnterpriseProbes("")
		svc.enterpriseState().wg.Wait()
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	svc.registerEnterpriseModels(t.Context(), auth)
	svc.enterpriseState().wg.Wait()
	policy := manager.EnterpriseModelPolicyView(auth)
	if policy.Subscription.Status != "ok" || policy.Subscription.FreeTrial == nil || *policy.Subscription.FreeTrial || !policy.Allows(ge.CarbonModel) {
		t.Fatal("paid Carbon subscription not verified")
	}
	trace := &carbonCapacityTransport{transport: http.DefaultTransport, records: map[string][]*carbonCapacityRecord{}}
	manager.SetRoundTripperProvider(trace)
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	gemini := geminihandlers.NewGeminiAPIHandler(base)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), carbonCapacityCaseKey{}, c.GetHeader("X-Capacity-Case")))
		c.Next()
	})
	router.GET("/v1beta/models", gemini.GeminiModels)
	router.POST("/v1beta/models/*action", gemini.GeminiHandler)
	router.GET("/__trace", func(c *gin.Context) {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		c.JSON(200, gin.H{"active": trace.active, "peak": trace.peak, "records": trace.records[c.Query("id")]})
	})
	router.POST("/__tokens", func(c *gin.Context) {
		var data struct {
			Text string `json:"text"`
		}
		if c.ShouldBindJSON(&data) != nil {
			c.Status(400)
			return
		}
		payload, _ := json.Marshal(map[string]any{"query": map[string]string{"text": data.Text}})
		detail := helps.NewEnterpriseTokenCounter(payload).Detail(nil)
		c.JSON(200, gin.H{"bytes": len(data.Text), "characters": utf8.RuneCountInString(data.Text), "local_gemma3_tokens": detail.InputTokens})
	})
	router.POST("/__native", func(c *gin.Context) {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil || !gjson.ValidBytes(body) || gjson.GetBytes(body, "generationSpec.modelId").String() != ge.CarbonModel {
			c.Status(400)
			return
		}
		url := ge.OriginForLocation(application.Location) + "/v1alpha/" + ge.Engine(application) + "/assistants/" + application.AssistantID + ":streamAssist"
		request, errRequest := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, bytes.NewReader(body))
		if errRequest != nil {
			c.Status(500)
			return
		}
		request.Header.Set("Content-Type", "application/json")
		ctx := context.WithValue(c.Request.Context(), "cliproxy.roundtripper", http.RoundTripper(trace))
		response, errHTTP := executor.HttpRequest(ctx, auth, request)
		if errHTTP != nil {
			c.JSON(502, gin.H{"error": fmt.Sprintf("native diagnostic: %T", errHTTP)})
			return
		}
		defer func() { _ = response.Body.Close() }()
		c.Status(response.StatusCode)
		_, _ = io.Copy(c.Writer, response.Body)
	})
	stop := make(chan struct{})
	var stopOnce sync.Once
	router.POST("/__stop", func(c *gin.Context) { stopOnce.Do(func() { close(stop) }); c.Status(204) })
	server := httptest.NewServer(router)
	defer server.Close()
	ready, _ := json.Marshal(map[string]any{"url": server.URL, "model": ge.CarbonModel, "subscription": policy.Subscription, "force_assist": input.ForceAssist, "started_at": time.Now().Format(time.RFC3339)})
	if os.WriteFile(input.Ready, ready, 0600) != nil {
		t.Fatal("could not publish the loopback endpoint")
	}
	select {
	case <-stop:
	case <-t.Context().Done():
	}
}

type carbonCapacityRecord struct {
	Started        string   `json:"started_at"`
	Finished       string   `json:"finished_at,omitempty"`
	Model          string   `json:"model"`
	InputBytes     int      `json:"input_bytes"`
	InputChars     int      `json:"input_characters"`
	GenerationKeys []string `json:"generation_keys"`
	HTTPStatus     int      `json:"http_status"`
	ResponseBytes  int64    `json:"response_bytes"`
	RetryAfter     string   `json:"retry_after,omitempty"`
	Error          any      `json:"error,omitempty"`
	States         []string `json:"states,omitempty"`
	CaptureLimited bool     `json:"capture_limited,omitempty"`
}

type carbonCapacityTransport struct {
	mu        sync.Mutex
	transport http.RoundTripper
	records   map[string][]*carbonCapacityRecord
	active    int
	peak      int
}

func (c *carbonCapacityTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return c }
func (c *carbonCapacityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, ":streamAssist") {
		return c.transport.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	id, _ := req.Context().Value(carbonCapacityCaseKey{}).(string)
	if id == "" {
		if gc, ok := req.Context().Value("gin").(*gin.Context); ok && gc != nil {
			id = gc.GetHeader("X-Capacity-Case")
		}
	}
	query := gjson.GetBytes(body, "query.text").String()
	record := &carbonCapacityRecord{Started: time.Now().Format(time.RFC3339Nano), Model: gjson.GetBytes(body, "generationSpec.modelId").String(), InputBytes: len(query), InputChars: utf8.RuneCountInString(query)}
	gjson.GetBytes(body, "generationSpec").ForEach(func(key, _ gjson.Result) bool {
		record.GenerationKeys = append(record.GenerationKeys, key.String())
		return true
	})
	sort.Strings(record.GenerationKeys)
	c.mu.Lock()
	c.records[id] = append(c.records[id], record)
	c.active++
	c.peak = max(c.active, c.peak)
	c.mu.Unlock()
	response, err := c.transport.RoundTrip(req)
	if err != nil {
		c.mu.Lock()
		record.Finished, record.Error = time.Now().Format(time.RFC3339Nano), fmt.Sprintf("%T", err)
		c.active--
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	record.HTTPStatus, record.RetryAfter = response.StatusCode, response.Header.Get("Retry-After")
	c.mu.Unlock()
	response.Body = &carbonCapacityBody{ReadCloser: response.Body, owner: c, record: record}
	return response, nil
}

type carbonCapacityBody struct {
	io.ReadCloser
	owner  *carbonCapacityTransport
	record *carbonCapacityRecord
	buffer bytes.Buffer
	count  int64
	once   sync.Once
}

func (b *carbonCapacityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.count += int64(n)
	if remaining := 2*1024*1024 - b.buffer.Len(); remaining > 0 {
		_, _ = b.buffer.Write(p[:min(n, remaining)])
	}
	return n, err
}
func (b *carbonCapacityBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		b.owner.mu.Lock()
		defer b.owner.mu.Unlock()
		b.record.Finished, b.record.ResponseBytes = time.Now().Format(time.RFC3339Nano), b.count
		b.record.CaptureLimited = b.count > int64(b.buffer.Len())
		data := gjson.ParseBytes(b.buffer.Bytes())
		if data.IsArray() {
			data.ForEach(func(_, entry gjson.Result) bool { b.collect(entry); return true })
		} else {
			b.collect(data)
		}
		b.owner.active--
	})
	return err
}
func (b *carbonCapacityBody) collect(data gjson.Result) {
	if value := data.Get("error"); value.Exists() {
		var safe map[string]any
		_ = json.Unmarshal([]byte(value.Raw), &safe)
		b.record.Error = safe
	}
	if value := data.Get("answer.state").String(); value != "" {
		if len(b.record.States) == 0 || b.record.States[len(b.record.States)-1] != value {
			b.record.States = append(b.record.States, value)
		}
	}
}
