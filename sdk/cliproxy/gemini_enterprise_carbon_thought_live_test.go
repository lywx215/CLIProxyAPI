package cliproxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexec "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	geminihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const carbonThoughtLiveProject = "1002653018111"
const carbonThoughtLiveEngine = "gemini-enterprise-17939735_1793973557773"

const carbonThoughtLivePrompt = "A fair six-sided die is rolled eight times. What is the exact probability that the sum is 28 and every face 1 through 6 appears at least once? Derive an exact reduced fraction and a decimal rounded to eight places, then give a concise verification. Do not use external search or tools."

type carbonThoughtArrival struct {
	end int64
	at  time.Time
}

type carbonThoughtRawMetrics struct {
	IncludeThoughts bool             `json:"include_thoughts"`
	IncludePresent  bool             `json:"include_thoughts_present"`
	HTTPStatus      int              `json:"http_status"`
	TotalMS         int64            `json:"total_ms"`
	FirstThoughtMS  *int64           `json:"first_thought_ms"`
	FirstBodyMS     *int64           `json:"first_body_ms"`
	ThoughtParts    int              `json:"nonempty_thought_parts"`
	BodyParts       int              `json:"nonempty_body_parts"`
	ThoughtBytes    int              `json:"thought_bytes"`
	BodyBytes       int              `json:"body_bytes"`
	ThoughtTokens   int64            `json:"visible_summary_gemma3_tokens"`
	BodyTokens      int64            `json:"body_gemma3_tokens"`
	BodyTokensPS    *float64         `json:"body_delivery_tokens_per_second"`
	ThoughtPaths    []string         `json:"thought_paths"`
	UsageNumbers    map[string]int64 `json:"reported_usage"`
	Terminal        string           `json:"terminal"`
	Complete        bool             `json:"complete"`
	started         time.Time
	finished        time.Time
	arrivals        []carbonThoughtArrival
	thoughts        strings.Builder
	body            strings.Builder
	session         string
	reportedCounter *helps.EnterpriseTokenCounter
	reportedThought *int64
	done            chan struct{}
}

func (m *carbonThoughtRawMetrics) arrival(offset int64) time.Time {
	for _, arrival := range m.arrivals {
		if arrival.end >= offset {
			return arrival.at
		}
	}
	return m.finished
}

func (m *carbonThoughtRawMetrics) text(text, path string, thought bool, at time.Time) {
	if text == "" {
		return
	}
	nonempty := strings.TrimSpace(text) != ""
	if thought {
		if nonempty {
			m.ThoughtParts++
		}
		m.ThoughtBytes += len(text)
		m.thoughts.WriteString(text)
		if m.FirstThoughtMS == nil && nonempty {
			value := at.Sub(m.started).Milliseconds()
			m.FirstThoughtMS = &value
		}
		if !slices.Contains(m.ThoughtPaths, path) {
			m.ThoughtPaths = append(m.ThoughtPaths, path)
		}
	} else {
		if nonempty {
			m.BodyParts++
		}
		m.BodyBytes += len(text)
		m.body.WriteString(text)
		if m.FirstBodyMS == nil && nonempty {
			value := at.Sub(m.started).Milliseconds()
			m.FirstBodyMS = &value
		}
	}
}

func (m *carbonThoughtRawMetrics) content(value gjson.Result, path string, inherited bool, at time.Time) {
	if value.IsArray() {
		value.ForEach(func(_, part gjson.Result) bool { m.content(part, path+"[]", inherited, at); return true })
		return
	}
	thought := inherited || value.Get("thought").Type == gjson.True
	if text := value.Get("text"); text.Type == gjson.String {
		m.text(text.String(), path+".text", thought, at)
	}
	for _, key := range []string{"parts", "content"} {
		if nested := value.Get(key); nested.Exists() {
			m.content(nested, path+"."+key, thought, at)
		}
	}
}

func (m *carbonThoughtRawMetrics) frame(raw []byte, offset int64) {
	data := gjson.ParseBytes(raw)
	if data.IsArray() {
		data.ForEach(func(_, item gjson.Result) bool { m.frame([]byte(item.Raw), offset); return true })
		return
	}
	at := m.arrival(offset)
	if state := data.Get("answer.state").String(); state != "" {
		m.Terminal = state
	}
	if session := data.Get("sessionInfo.session").String(); session != "" {
		m.session = session
	}
	if usage := data.Get("usageMetadata"); usage.Exists() {
		m.reportedCounter.ObserveUsage(json.RawMessage(usage.Raw))
	}
	for _, root := range []string{"usageMetadata", "answer.usageMetadata"} {
		for _, key := range []string{"promptTokenCount", "candidatesTokenCount", "thoughtsTokenCount", "totalTokenCount", "cachedContentTokenCount"} {
			value := data.Get(root + "." + key)
			if value.Type == gjson.Number && value.Int() >= 0 {
				m.UsageNumbers[root+"."+key] = value.Int()
			}
		}
	}
	data.Get("answer.replies").ForEach(func(_, reply gjson.Result) bool {
		inherited := reply.Get("thought").Type == gjson.True
		if grounded := reply.Get("groundedContent.content"); grounded.Exists() {
			m.content(grounded, "answer.replies[].groundedContent.content", inherited || reply.Get("groundedContent.thought").Type == gjson.True, at)
		} else if content := reply.Get("content"); content.Exists() {
			m.content(content, "answer.replies[].content", inherited, at)
		}
		return true
	})
}

func (m *carbonThoughtRawMetrics) parse(data []byte) {
	m.UsageNumbers = map[string]int64{}
	m.reportedCounter = helps.NewEnterpriseTokenCounter([]byte(`{}`))
	decoder := json.NewDecoder(bytes.NewReader(data))
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return
	}
	if trimmed[0] == '[' {
		if _, err := decoder.Token(); err != nil {
			return
		}
		for decoder.More() {
			var raw json.RawMessage
			if decoder.Decode(&raw) != nil {
				return
			}
			m.frame(raw, decoder.InputOffset())
		}
		if _, err := decoder.Token(); err != nil {
			return
		}
	}
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}
		m.frame(raw, decoder.InputOffset())
	}
	m.Complete = m.Terminal == "SUCCEEDED"
	m.TotalMS = m.finished.Sub(m.started).Milliseconds()
	count := func(text string) int64 {
		payload, _ := json.Marshal(map[string]any{"query": map[string]string{"text": text}})
		return helps.NewEnterpriseTokenCounter(payload).Detail(nil).InputTokens
	}
	m.ThoughtTokens, m.BodyTokens = count(m.thoughts.String()), count(m.body.String())
	reported := m.reportedCounter.Detail(nil)
	if reported.TokenPresence != nil && reported.TokenPresence.ReasoningTokens {
		value := reported.ReasoningTokens
		m.reportedThought = &value
	}
	if m.FirstBodyMS != nil {
		seconds := m.finished.Sub(m.started).Seconds() - float64(*m.FirstBodyMS)/1000
		if seconds > 0 {
			rate := float64(m.BodyTokens) / seconds
			m.BodyTokensPS = &rate
		}
	}
}

type carbonThoughtTrace struct {
	mu        sync.Mutex
	transport http.RoundTripper
	engine    string
	limit     int
	records   []*carbonThoughtRawMetrics
	caseID    string
	cases     map[string]bool
}

func (c *carbonThoughtTrace) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return c }

func (c *carbonThoughtTrace) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		expected := "/v1alpha/" + c.engine + "/assistants/default_assistant:streamAssist"
		if request.Method != http.MethodPost || request.URL.Path != expected || !ge.OfficialBinding(request, carbonThoughtLiveProject, "au") {
			return nil, errors.New("live thought test forbids unrelated writes")
		}
		data, err := io.ReadAll(io.LimitReader(request.Body, 2*1024*1024+1))
		if err != nil || len(data) > 2*1024*1024 || gjson.GetBytes(data, "generationSpec.modelId").String() != ge.CarbonModel {
			return nil, errors.New("live thought test requires fixed Carbon payload")
		}
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(data))
		c.mu.Lock()
		if len(c.records) >= c.limit || c.cases[c.caseID] {
			c.mu.Unlock()
			return nil, errors.New("live thought generation limit reached")
		}
		if c.cases == nil {
			c.cases = map[string]bool{}
		}
		include := gjson.GetBytes(data, "generationSpec.thinkingConfig.includeThoughts")
		expectedInclude := !strings.Contains(c.caseID, "-false")
		if include.Type != gjson.True && include.Type != gjson.False || (include.Type == gjson.True) != expectedInclude {
			c.mu.Unlock()
			return nil, errors.New("live thought final visibility field did not match its fixed test case")
		}
		c.cases[c.caseID] = true
		record := &carbonThoughtRawMetrics{IncludeThoughts: include.Type == gjson.True, IncludePresent: include.Type == gjson.True || include.Type == gjson.False, started: time.Now(), done: make(chan struct{})}
		c.records = append(c.records, record)
		c.mu.Unlock()
		response, err := c.transport.RoundTrip(request)
		if err != nil {
			close(record.done)
			return nil, errors.New("live thought upstream transport failed")
		}
		record.HTTPStatus = response.StatusCode
		response.Body = &carbonThoughtBody{ReadCloser: response.Body, record: record}
		return response, nil
	}
	return c.transport.RoundTrip(request)
}

type carbonThoughtBody struct {
	io.ReadCloser
	record *carbonThoughtRawMetrics
	buffer bytes.Buffer
	read   int64
	once   sync.Once
}

func (b *carbonThoughtBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.read += int64(n)
		b.record.arrivals = append(b.record.arrivals, carbonThoughtArrival{b.read, time.Now()})
		if b.buffer.Len()+n <= 4*1024*1024 {
			_, _ = b.buffer.Write(p[:n])
		}
	}
	return n, err
}

func (b *carbonThoughtBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		defer close(b.record.done)
		b.record.finished = time.Now()
		if b.read == int64(b.buffer.Len()) {
			b.record.parse(b.buffer.Bytes())
		}
		b.buffer.Reset()
	})
	return err
}

// This opt-in harness reads only process-memory OAuth material and writes only
// sanitized counts and timings. It never enables Google or deployed applications.
func TestEnterpriseCarbonThoughtLive(t *testing.T) {
	raw := os.Getenv("CLIPROXY_ENTERPRISE_CARBON_THOUGHT_LIVE_JSON")
	if raw == "" {
		t.Skip("requires explicit isolated Carbon thought-test authorization")
	}
	var input struct {
		Metadata map[string]any `json:"metadata"`
		Mode     string         `json:"mode"`
		Report   string         `json:"report"`
	}
	if json.Unmarshal([]byte(raw), &input) != nil || input.Report == "" || input.Mode != "direct" && input.Mode != "system" {
		t.Fatal("invalid isolated thought-test input")
	}
	binding := ge.FromMetadata(input.Metadata)
	if binding.Validate() != nil || binding.ProjectID != carbonThoughtLiveProject || binding.Location != "au" || binding.EngineID != carbonThoughtLiveEngine || binding.CollectionID != "default_collection" || binding.AssistantID != "default_assistant" {
		t.Fatal("isolated thought test requires its authorized AU application")
	}
	report := map[string]any{"started_at": time.Now().Format(time.RFC3339), "mode": input.Mode, "model": ge.CarbonModel, "project_id": binding.ProjectID, "location": binding.Location, "engine_id": binding.EngineID, "credential_store": "memory_only"}
	defer func() {
		report["finished_at"], report["passed"] = time.Now().Format(time.RFC3339), !t.Failed()
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(input.Report, append(data, '\n'), 0600) != nil {
			t.Error("could not save sanitized thought-test metrics")
		}
	}()
	client, err := ge.Client("")
	if err != nil {
		t.Fatal("could not create isolated Google client")
	}
	defer client.CloseIdleConnections()
	token := ge.String(input.Metadata, "access_token")
	observation := ge.ResolveSubscription(t.Context(), client, binding, token, time.Now())
	report["subscription_status"], report["subscription_free_trial"], report["subscription_edition"] = observation.Status, observation.FreeTrial, observation.Edition
	if observation.Status != "ok" || observation.FreeTrial == nil || observation.State != "ACTIVE" {
		t.Fatal("assigned active AU subscription was not verified")
	}
	if input.Mode == "system" {
		carbonThoughtSystemLive(t, input.Metadata, observation, report)
		return
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	trace := &carbonThoughtTrace{transport: transport, engine: ge.Engine(binding), limit: 2}
	client.Transport = trace
	report["requests"] = &trace.records
	for _, include := range []bool{true, false} {
		trace.caseID = map[bool]string{true: "direct-true", false: "direct-false"}[include]
		payload, _ := json.Marshal(map[string]any{"query": map[string]string{"text": carbonThoughtLivePrompt}, "generationSpec": map[string]any{"modelId": ge.CarbonModel, "thinkingConfig": map[string]bool{"includeThoughts": include}}, "assistSkippingMode": "REQUEST_ASSIST"})
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ge.OriginForLocation(binding.Location)+"/v1alpha/"+trace.engine+"/assistants/default_assistant:streamAssist", bytes.NewReader(payload))
		if err != nil {
			t.Fatal("could not construct fixed AU generation")
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Goog-User-Project", binding.ProjectID)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("direct generation failed; it will not be replayed")
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			t.Fatal("direct AU generation did not complete successfully")
		}
	}
	if len(trace.records) != 2 || !trace.records[0].Complete || !trace.records[1].Complete || trace.records[0].BodyBytes == 0 || trace.records[1].BodyBytes == 0 {
		t.Fatal("direct AU responses lack complete generated bodies")
	}
}

type carbonThoughtClientMetrics struct {
	Case                         string   `json:"case"`
	PrivateBootstrap             bool     `json:"private_fixed_application_bootstrap"`
	Stream                       bool     `json:"stream"`
	IncludeThoughts              bool     `json:"include_thoughts"`
	HTTPStatus                   int      `json:"http_status"`
	TotalMS                      int64    `json:"total_ms"`
	FirstThoughtMS               *int64   `json:"first_thought_ms"`
	FirstBodyMS                  *int64   `json:"first_body_ms"`
	FirstThoughtEvent            *int     `json:"first_thought_event"`
	FirstBodyEvent               *int     `json:"first_body_event"`
	ThoughtParts                 int      `json:"returned_summary_parts"`
	ThoughtBytes                 int      `json:"returned_summary_bytes"`
	ReturnedSummaryTokens        int64    `json:"returned_summary_tokens"`
	ReturnedSummaryTokenSource   string   `json:"returned_summary_token_source"`
	BodyTokens                   int64    `json:"returned_body_tokens"`
	BodyTokensPS                 *float64 `json:"body_delivery_tokens_per_second"`
	InterfaceReasoning           *int64   `json:"interface_thoughts_token_count"`
	InterfaceReasoningSource     string   `json:"interface_thoughts_token_source"`
	SummaryMatchesRaw            bool     `json:"summary_matches_raw_thought_text"`
	SummaryLeakedInBody          bool     `json:"summary_leaked_in_body"`
	Terminal                     bool     `json:"terminal_stop"`
	ReadComplete                 bool     `json:"read_complete"`
	events                       int
	started, firstBody, lastBody time.Time
	thoughts, body               strings.Builder
}

func (m *carbonThoughtClientMetrics) frame(raw []byte) {
	data := gjson.ParseBytes(raw)
	if data.IsArray() {
		data.ForEach(func(_, frame gjson.Result) bool { m.frame([]byte(frame.Raw)); return true })
		return
	}
	if usage := data.Get("usageMetadata.thoughtsTokenCount"); usage.Type == gjson.Number {
		value := usage.Int()
		m.InterfaceReasoning = &value
	}
	data.Get("candidates").ForEach(func(_, candidate gjson.Result) bool {
		m.Terminal = m.Terminal || candidate.Get("finishReason").String() == "STOP"
		candidate.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
			text := part.Get("text").String()
			if text == "" {
				return true
			}
			if strings.TrimSpace(text) == "" {
				if part.Get("thought").Type == gjson.True {
					m.thoughts.WriteString(text)
					m.ThoughtBytes += len(text)
				} else {
					m.body.WriteString(text)
				}
				return true
			}
			m.events++
			at := time.Now()
			if part.Get("thought").Type == gjson.True {
				m.ThoughtParts++
				m.ThoughtBytes += len(text)
				m.thoughts.WriteString(text)
				if m.FirstThoughtEvent == nil {
					event := m.events
					m.FirstThoughtEvent = &event
					if m.Stream {
						value := at.Sub(m.started).Milliseconds()
						m.FirstThoughtMS = &value
					}
				}
			} else {
				m.body.WriteString(text)
				m.lastBody = at
				if m.FirstBodyEvent == nil {
					event := m.events
					m.FirstBodyEvent = &event
					m.firstBody = at
					if m.Stream {
						value := at.Sub(m.started).Milliseconds()
						m.FirstBodyMS = &value
					}
				}
			}
			return true
		})
		return true
	})
}

func (m *carbonThoughtClientMetrics) read(response *http.Response) error {
	if !m.Stream {
		data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
		if err != nil || len(data) > 4*1024*1024 || !gjson.ValidBytes(data) {
			return errors.New("incomplete local nonstream response")
		}
		m.frame(data)
		m.ReadComplete = true
		return nil
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data:")) {
			frame := bytes.TrimSpace(line[len("data:"):])
			if bytes.Equal(frame, []byte("[DONE]")) {
				continue
			}
			if !gjson.ValidBytes(frame) || gjson.GetBytes(frame, "error").Exists() {
				return errors.New("local SSE returned an invalid or failed event")
			}
			m.frame(frame)
		}
	}
	m.ReadComplete = scanner.Err() == nil
	return scanner.Err()
}

func carbonThoughtSystemLive(t *testing.T, metadata map[string]any, observation ge.SubscriptionObservation, report map[string]any) {
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 1)
	manager.RegisterExecutor(runtimeexec.NewGeminiEnterpriseExecutor(cfg))
	metadata[ge.CarbonEnabledKey] = false
	delete(metadata, ge.CarbonEvidenceKey)
	delete(metadata, ge.CarbonEvidenceStoreKey)
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: "carbon-thought-isolated.json", FileName: "carbon-thought-isolated.json", Provider: ge.Provider, Status: coreauth.StatusActive, Metadata: metadata})
	if err != nil {
		t.Fatal("could not register in-memory thought-test authorization")
	}
	flight := manager.BeginEnterpriseSubscriptionRefresh(auth)
	if flight == nil || !flight.Complete(observation) {
		t.Fatal("could not bind the verified assigned subscription")
	}
	manager.PublishEnterpriseSubscriptionIdentity(auth)
	auth, _ = manager.GetByID(auth.ID)
	binding := ge.FromMetadata(auth.Metadata)
	trace := &carbonThoughtTrace{transport: http.DefaultTransport, engine: ge.Engine(binding), limit: 4}
	manager.SetRoundTripperProvider(trace)
	report["raw_upstream_requests"] = &trace.records
	results := []*carbonThoughtClientMetrics{}
	report["requests"] = &results
	lease, err := manager.BeginEnterpriseCarbonTest(auth, 1)
	if err != nil {
		t.Fatal("could not create fixed-application bootstrap lease")
	}
	defer manager.CancelEnterpriseCarbonTest(lease)
	bootstrap := true
	bootstrapAuthID := auth.ID
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
	public := geminihandlers.NewGeminiAPIHandler(base)
	router := gin.New()
	router.POST("/__carbon_thought_private_bootstrap", func(c *gin.Context) {
		body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, 64*1024))
		if errRead != nil || !gjson.ValidBytes(body) {
			c.Status(400)
			return
		}
		ctx := coreauth.WithEnterpriseCarbonModelTest(c.Request.Context(), bootstrapAuthID, ge.Resources(binding))
		result, err := manager.ExecuteStream(ctx, []string{ge.Provider}, coreexec.Request{Model: ge.CarbonModel, Payload: body}, coreexec.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, OriginalRequest: body, Stream: true, Metadata: map[string]any{coreexec.PinnedAuthMetadataKey: bootstrapAuthID, coreexec.OperationMetadataKey: "generate_content", "source": "isolated_carbon_thought_bootstrap"}})
		if err != nil {
			c.JSON(503, gin.H{"error": gin.H{"code": "private_bootstrap_unavailable"}})
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.Status(200)
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				_, _ = c.Writer.Write([]byte("event: error\ndata: {\"error\":{\"code\":\"private_bootstrap_failed\"}}\n\n"))
				c.Writer.Flush()
				return
			}
			_, _ = c.Writer.Write([]byte("data: "))
			_, _ = c.Writer.Write(chunk.Payload)
			_, _ = c.Writer.Write([]byte("\n\n"))
			c.Writer.Flush()
		}
	})
	router.POST("/v1beta/models/*action", public.GeminiHandler)
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	svc := &Service{cfg: cfg, coreManager: manager, antigravityContext: t.Context()}
	t.Cleanup(func() {
		svc.cancelEnterpriseProbes("")
		svc.enterpriseState().wg.Wait()
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
	for index, testCase := range []struct {
		name            string
		stream, include bool
	}{
		{"gemini-stream-true-private-bootstrap", true, true},
		{"gemini-stream-true-business", true, true},
		{"gemini-nonstream-true-business", false, true},
		{"gemini-stream-false-business", true, false},
	} {
		trace.mu.Lock()
		trace.caseID = testCase.name
		trace.mu.Unlock()
		nonce := "THOUGHT_VERIFY_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		prompt := carbonThoughtLivePrompt + " End your final response with this verification marker: " + nonce
		payload, _ := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": prompt}}}}, "generationConfig": map[string]any{"thinkingConfig": map[string]bool{"includeThoughts": testCase.include}}})
		metrics := &carbonThoughtClientMetrics{Case: testCase.name, PrivateBootstrap: bootstrap, Stream: testCase.stream, IncludeThoughts: testCase.include, started: time.Now(), ReturnedSummaryTokenSource: "local_gemma3_sentencepiece", InterfaceReasoningSource: "unavailable"}
		results = append(results, metrics)
		action := ":generateContent"
		if testCase.stream {
			action = ":streamGenerateContent?alt=sse"
		}
		endpoint := server.URL + "/v1beta/models/" + ge.CarbonModel + action
		if bootstrap {
			endpoint = server.URL + "/__carbon_thought_private_bootstrap"
		}
		response, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal("local thought-test request failed and will not be replayed")
		}
		metrics.HTTPStatus = response.StatusCode
		errRead := metrics.read(response)
		_ = response.Body.Close()
		metrics.TotalMS = time.Since(metrics.started).Milliseconds()
		trace.mu.Lock()
		if len(trace.records) != index+1 {
			trace.mu.Unlock()
			t.Fatal("local request did not submit exactly one fixed Carbon generation")
		}
		raw := trace.records[index]
		trace.mu.Unlock()
		<-raw.done
		count := func(text string) int64 {
			payload, _ := json.Marshal(map[string]any{"query": map[string]string{"text": text}})
			return helps.NewEnterpriseTokenCounter(payload).Detail(nil).InputTokens
		}
		metrics.ReturnedSummaryTokens, metrics.BodyTokens = count(metrics.thoughts.String()), count(metrics.body.String())
		metrics.SummaryMatchesRaw = sha256.Sum256([]byte(metrics.thoughts.String())) == sha256.Sum256([]byte(raw.thoughts.String()))
		metrics.SummaryLeakedInBody = raw.thoughts.Len() > 0 && strings.Contains(metrics.body.String(), raw.thoughts.String())
		if metrics.Stream && metrics.lastBody.After(metrics.firstBody) {
			rate := float64(metrics.BodyTokens) / metrics.lastBody.Sub(metrics.firstBody).Seconds()
			metrics.BodyTokensPS = &rate
		}
		// This records the counter's selected source from raw field availability
		// and the executor's fallback rules, never by comparing numeric values.
		if raw.reportedThought != nil {
			metrics.InterfaceReasoningSource = "upstream_usageMetadata_thoughtsTokenCount"
		} else if raw.ThoughtBytes > 0 {
			metrics.InterfaceReasoningSource = "local_upstream_thought_summary_gemma3"
		}
		if errRead != nil || metrics.HTTPStatus != 200 || !metrics.Terminal || !metrics.ReadComplete || !raw.Complete || raw.HTTPStatus != 200 || !raw.IncludePresent || raw.IncludeThoughts != testCase.include || metrics.body.Len() == 0 || metrics.SummaryLeakedInBody {
			t.Fatal("local thought-test response failed fixed payload, complete delivery or privacy checks")
		}
		if testCase.include && (metrics.ThoughtParts == 0 || raw.ThoughtParts == 0 || !metrics.SummaryMatchesRaw) {
			t.Fatal("includeThoughts true did not deliver actual nonempty upstream thought text")
		}
		if !testCase.include && metrics.ThoughtBytes != 0 {
			t.Fatal("includeThoughts false leaked thought-marked text")
		}
		if bootstrap {
			if !strings.Contains(metrics.body.String(), nonce) || !carbonThoughtVerifyActualModel(t, metadata, raw.session, nonce) {
				t.Fatal("bootstrap could not verify the same session's actual Carbon model")
			}
			root, err := manager.RecordEnterpriseCarbonTestEvidence(t.Context(), lease, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", HTTPStatus: 200, UpstreamModel: ge.CarbonModel}})
			if err != nil {
				t.Fatal("could not record actual bootstrap verification in memory")
			}
			auth, _ = manager.GetByID(root.ID)
			root, err = manager.UpdateEnterpriseCarbonEnabled(t.Context(), auth, 1, true, lease.TestID)
			if err != nil {
				t.Fatal("could not enable the verified application in isolated memory")
			}
			auth = manager.EnterpriseCollections(root.ID)[0]
			svc.registerEnterpriseModels(t.Context(), auth)
			svc.enterpriseState().wg.Wait()
			bootstrap = false
			report["bootstrap_actual_model_verified"], report["bootstrap_memory_only_admission"] = true, true
		}
		t.Logf("case=%s thought_parts=%d local_summary_tokens=%d total_ms=%d", metrics.Case, metrics.ThoughtParts, metrics.ReturnedSummaryTokens, metrics.TotalMS)
	}
	current, _ := manager.GetByID(auth.ID)
	accountRoot, account, ok := manager.EnterpriseAccountSnapshot(current.ID)
	if !ok || len(account.Collections) != 1 {
		t.Fatal("isolated application changed unexpectedly")
	}
	if _, err := manager.UpdateEnterpriseCarbonEnabled(t.Context(), current, account.Collections[0].Revision, false, ""); err != nil {
		t.Fatal("could not close isolated memory-only admission")
	}
	report["application_enabled_at_end"] = false
	report["source_store_used"] = false
	_ = accountRoot
}

func carbonThoughtVerifyActualModel(t *testing.T, metadata map[string]any, session, nonce string) bool {
	t.Helper()
	binding := ge.FromMetadata(metadata)
	prefix := ge.Engine(binding) + "/sessions/"
	if !strings.HasPrefix(session, prefix) || session == prefix || strings.ContainsAny(strings.TrimPrefix(session, prefix), "/?#%") {
		return false
	}
	client, err := ge.Client("")
	if err != nil {
		return false
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), "GET", ge.OriginForLocation(binding.Location)+"/v1alpha/"+session+"?fields="+url.QueryEscape("name,turns(query(text),queryConfig)"), nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+ge.String(metadata, "access_token"))
	request.Header.Set("X-Goog-User-Project", binding.ProjectID)
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || response.StatusCode != 200 || len(data) > 2*1024*1024 || gjson.GetBytes(data, "name").String() != session {
		return false
	}
	verified := false
	gjson.GetBytes(data, "turns").ForEach(func(_, turn gjson.Result) bool {
		if strings.Contains(turn.Get("query.text").String(), nonce) {
			var config map[string]any
			if json.Unmarshal([]byte(turn.Get("queryConfig").Raw), &config) == nil && config["google.discoveryengine.googleapis.com.Assistant.model_id"] == ge.CarbonModel {
				verified = true
			}
		}
		return true
	})
	return verified
}
