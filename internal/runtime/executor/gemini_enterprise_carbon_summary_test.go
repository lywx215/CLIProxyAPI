package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

// The fixture uses the ordinary Carbon admission path, including persisted proof
// and the explicit application switch. No management-probe bypass is installed.
func carbonSummaryApplication(t *testing.T, cfg *config.Config, prefix string) (*coreauth.Manager, *coreauth.Auth) {
	t.Helper()
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	auth := carbonSummaryAddApplication(t, m, prefix, "summary-app")
	m.RegisterExecutor(NewGeminiEnterpriseExecutor(cfg))
	return m, auth
}

func carbonSummaryAddApplication(t *testing.T, m *coreauth.Manager, prefix, engine string) *coreauth.Auth {
	t.Helper()
	id := "carbon-summary-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + engine
	binding := config.GeminiEnterpriseOAuth{ClientID: "summary-client", ProjectID: "123", EngineID: engine}.WithDefaults()
	email := "carbon-summary-source@example.test"
	if engine != "summary-app" {
		email = "carbon-summary-spare@example.test"
	}
	auth := &coreauth.Auth{ID: id, FileName: id + ".json", Provider: ge.Provider, Prefix: prefix, Metadata: ge.Metadata(binding, email, &oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})}
	auth.Metadata["model_policy"] = "catalog"
	auth.EnterpriseQuotaIdentity = &ge.QuotaIdentity{Verified: true, Binding: ge.Binding(binding), ProjectNumber: "123", Location: binding.Location, Edition: "enterprise_plus", PrincipalHash: strings.Repeat("a", 64), CheckedAt: time.Now()}
	registered, err := m.Register(t.Context(), auth)
	if err != nil {
		t.Fatal(err)
	}
	trial := false
	flight := m.BeginEnterpriseSubscriptionRefresh(registered)
	if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial, State: "ACTIVE", ProjectNumber: "123", PrincipalHash: strings.Repeat("a", 64), CheckedAt: time.Now()}) {
		t.Fatal("verified subscription fixture was not published")
	}
	m.PublishEnterprisePolicyCatalog(registered, []ge.Model{{ID: ge.ArgonModel}, {ID: ge.CarbonModel}})
	account, err := ge.AccountFromLegacy(registered.Metadata)
	if err != nil || len(account.Collections) != 1 {
		t.Fatalf("application fixture: %v", err)
	}
	revision := account.Collections[0].Revision
	lease, err := m.BeginEnterpriseCarbonTest(registered, revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RecordEnterpriseCarbonTestEvidence(t.Context(), lease, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.CarbonModel, HTTPStatus: 200}}); err != nil {
		t.Fatal(err)
	}
	current, _ := m.GetByID(registered.ID)
	if _, err = m.UpdateEnterpriseCarbonEnabled(t.Context(), current, revision, true, lease.TestID); err != nil {
		t.Fatal(err)
	}
	current, _ = m.GetByID(registered.ID)
	if children := m.EnterpriseCollections(registered.ID); len(children) == 1 {
		current = children[0]
	}
	if current == nil || !m.EnterpriseCarbonAdmissionView(current).Available {
		t.Fatalf("ordinary Carbon admission was not established: %+v", m.EnterpriseCarbonAdmissionView(current))
	}
	registry.GetGlobalRegistry().RegisterClient(current.ID, ge.Provider, []*registry.ModelInfo{{ID: ge.CarbonModel, Type: "gemini", MetadataModelID: ge.CarbonModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(current.ID) })
	return current
}

func carbonSummaryRequest(format translator.Format, visibility string) []byte {
	body := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hi"}}}}}
	if format == translator.FormatOpenAI {
		body = map[string]any{"messages": []any{map[string]string{"role": "user", "content": "hi"}}, "stream_options": map[string]bool{"include_usage": true}}
		if visibility != "absent" {
			body["extra_body"] = map[string]any{"google": map[string]any{"thinking_config": map[string]bool{"include_thoughts": visibility == "true"}}}
		}
	} else if visibility != "absent" {
		body["generationConfig"] = map[string]any{"thinkingConfig": map[string]bool{"includeThoughts": visibility == "true"}}
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

func carbonSummaryPackets(t *testing.T, e *GeminiEnterpriseExecutor, ctx context.Context, auth *coreauth.Auth, model string, body []byte, format translator.Format, stream bool) ([][]byte, error) {
	t.Helper()
	options := enterpriseOptions(format)
	options.Stream = stream
	req := coreexec.Request{Model: model, Payload: body}
	if !stream {
		response, err := e.Execute(ctx, auth, req, options)
		if err != nil {
			return nil, err
		}
		return [][]byte{response.Payload}, nil
	}
	response, err := e.ExecuteStream(ctx, auth, req, options)
	if err != nil {
		return nil, err
	}
	var packets [][]byte
	for chunk := range response.Chunks {
		if chunk.Err != nil {
			err = chunk.Err
		} else {
			packets = append(packets, chunk.Payload)
		}
	}
	return packets, err
}

func carbonSummaryText(packets [][]byte, format translator.Format, stream bool) (thought, text string, terminals int) {
	for _, raw := range packets {
		packet := gjson.ParseBytes(raw)
		if format == translator.FormatGemini {
			for _, part := range packet.Get("candidates.0.content.parts").Array() {
				if part.Get("thought").Bool() {
					thought += part.Get("text").String()
				} else {
					text += part.Get("text").String()
				}
			}
			if packet.Get("candidates.0.finishReason").String() == "STOP" {
				terminals++
			}
		} else {
			root := "choices.0.message."
			if stream {
				root = "choices.0.delta."
			}
			thought += packet.Get(root + "reasoning_content").String()
			text += packet.Get(root + "content").String()
			if packet.Get("choices.0.finish_reason").String() == "stop" {
				terminals++
			}
		}
	}
	return
}

type carbonSummaryUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c *carbonSummaryUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.AuthID == c.authID {
		c.records <- record
	}
}

func TestEnterpriseCarbonSummaryProtocolsVisibilityAndUsage(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, visibility := range []string{"true", "false", "absent"} {
				for _, scenario := range []string{"summary", "reported", "none", "reported-no-summary"} {
					t.Run(fmt.Sprintf("%s/stream=%v/%s/%s", format.String(), stream, visibility, scenario), func(t *testing.T) {
						cfg := &config.Config{}
						_, auth := carbonSummaryApplication(t, cfg, "")
						capture := &carbonSummaryUsageCapture{authID: auth.ID, records: make(chan usage.Record, 2)}
						usage.RegisterNamedPlugin(t.Name(), capture)
						t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), enterpriseUsageNoop{}) })
						upstream, wantText, wantThought := enterpriseThoughtAnswer, `{"ok":true}`, "summary 你好🙂summary continued"
						if scenario == "none" || scenario == "reported-no-summary" {
							upstream, wantText, wantThought = enterpriseAnswer, "hello", ""
						}
						if scenario == "reported" || scenario == "reported-no-summary" {
							upstream = strings.TrimSuffix(upstream, "]") + `,{"usageMetadata":{"thoughtsTokenCount":17}}]`
						}
						var calls atomic.Int32
						ctx := contextWithEnterpriseTransport(t, func(request *http.Request) (*http.Response, error) {
							calls.Add(1)
							wire, _ := io.ReadAll(request.Body)
							flag := gjson.GetBytes(wire, "generationSpec.thinkingConfig.includeThoughts")
							if flag.Exists() != (visibility != "absent") || visibility != "absent" && flag.Bool() != (visibility == "true") || gjson.GetBytes(wire, "generationSpec.modelId").String() != ge.CarbonModel {
								t.Errorf("summary intent not preserved: %s", wire)
							}
							if gjson.GetBytes(wire, "generationSpec.thinkingConfig.thinkingLevel").Exists() || gjson.GetBytes(wire, "generationSpec.thinkingConfig.thinkingBudget").Exists() {
								t.Error("Carbon gained unverified thinking strength")
							}
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstream))}, nil
						})
						packets, err := carbonSummaryPackets(t, NewGeminiEnterpriseExecutor(cfg), ctx, auth, ge.CarbonModel, carbonSummaryRequest(format, visibility), format, stream)
						if err != nil {
							t.Fatal(err)
						}
						thought, text, terminals := carbonSummaryText(packets, format, stream)
						if visibility != "true" {
							wantThought = ""
						}
						if calls.Load() != 1 || thought != wantThought || text != wantText || terminals != 1 {
							t.Fatalf("calls=%d thought=%q text=%q terminals=%d", calls.Load(), thought, text, terminals)
						}
						var record usage.Record
						select {
						case record = <-capture.records:
						case <-time.After(5 * time.Second):
							t.Fatal("missing usage settlement")
						}
						p := record.Detail.TokenPresence
						if record.Source != "carbon-summary-source@example.test" || record.Detail.Estimate != nil {
							t.Fatalf("summary provenance replaced account/estimate source: %+v", record)
						}
						known := scenario != "none"
						if p == nil || !p.InputTokens || !p.OutputTokens || !p.TotalTokens || p.CachedTokens || p.ReasoningTokens != known || record.Failed {
							t.Fatalf("wrong availability: %+v", record.Detail)
						}
						counter := helps.NewEnterpriseTokenCounter([]byte(`{"query":{"text":"hi"}}`))
						counter.ObserveThought("summary 你好🙂summary continued")
						wantReasoning := counter.Detail(nil).ReasoningTokens
						if scenario == "reported" || scenario == "reported-no-summary" {
							wantReasoning = 17
						} else if scenario == "none" {
							wantReasoning = 0
						}
						if record.Detail.ReasoningTokens != wantReasoning || record.Detail.TotalTokens != record.Detail.InputTokens+record.Detail.OutputTokens+wantReasoning {
							t.Fatalf("wrong summary accounting: %+v", record.Detail)
						}
						usagePackets := 0
						for _, packet := range packets {
							if strings.Contains(string(packet), "opaque-secret") || strings.Contains(string(packet), "usage_source") {
								t.Fatal("private/session/provenance data leaked")
							}
							path := "usageMetadata.thoughtsTokenCount"
							if format == translator.FormatOpenAI {
								path = "usage.completion_tokens_details.reasoning_tokens"
							}
							usageRoot := "usageMetadata"
							if format == translator.FormatOpenAI {
								usageRoot = "usage"
							}
							if gjson.GetBytes(packet, usageRoot).Exists() {
								usagePackets++
								if gjson.GetBytes(packet, path).Exists() != known || known && gjson.GetBytes(packet, path).Int() != wantReasoning {
									t.Fatalf("wire usage differs from settlement: %s", packet)
								}
							}
						}
						if usagePackets != 1 {
							t.Fatalf("usage packets=%d", usagePackets)
						}
					})
				}
			}
		}
	}
}

func TestEnterpriseCarbonSummaryFinalPayloadBarrier(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, filter := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/filter=%v", format, stream, filter), func(t *testing.T) {
					models := []config.PayloadModelRule{{Name: ge.CarbonModel, Protocol: ge.Provider}}
					cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{
						{Models: []config.PayloadModelRule{{Name: ge.CarbonModel, Protocol: ge.Provider, Match: []map[string]any{{"languageCode": "en"}}}}, Params: map[string]any{"languageCode": "fr"}},
						{Models: []config.PayloadModelRule{{Name: ge.CarbonModel, Protocol: ge.Provider, NotExist: []string{"languageCode"}}}, Params: map[string]any{"languageCode": "en"}},
						{Models: models, Params: map[string]any{"generationSpec.thinkingConfig.includeThoughts": false}},
					}}}
					if filter {
						cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"generationSpec.thinkingConfig"}}}
					}
					_, auth := carbonSummaryApplication(t, cfg, "")
					var calls atomic.Int32
					ctx := contextWithEnterpriseTransport(t, func(request *http.Request) (*http.Response, error) {
						calls.Add(1)
						wire, _ := io.ReadAll(request.Body)
						if gjson.GetBytes(wire, "languageCode").String() != "en" {
							t.Error("payload rules ran more than once")
						}
						if filter && gjson.GetBytes(wire, "generationSpec.thinkingConfig").Exists() || !filter && gjson.GetBytes(wire, "generationSpec.thinkingConfig.includeThoughts").Type != gjson.False {
							t.Errorf("final user setting was overwritten: %s", wire)
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseThoughtAnswer))}, nil
					})
					packets, err := carbonSummaryPackets(t, NewGeminiEnterpriseExecutor(cfg), ctx, auth, ge.CarbonModel, carbonSummaryRequest(format, "true"), format, stream)
					if err != nil {
						t.Fatal(err)
					}
					thought, _, terminal := carbonSummaryText(packets, format, stream)
					if thought != "" || terminal != 1 || calls.Load() != 1 {
						t.Fatalf("final visibility/replay mismatch: thought=%q terminal=%d calls=%d", thought, terminal, calls.Load())
					}
				})
			}
		}
	}
}

func TestEnterpriseCarbonSummaryAliasPrefixSuffixAndPinnedApplication(t *testing.T) {
	for _, model := range []string{ge.CarbonModel, ge.CarbonModel + "(high)", ge.CarbonModel + "(8192)", "carbon-friendly", "team/carbon-friendly(none)", "team/" + ge.CarbonModel + "(high)"} {
		for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", model, format, stream), func(t *testing.T) {
					prefix := ""
					if strings.HasPrefix(model, "team/") {
						prefix = "team"
					}
					cfg := &config.Config{}
					m, auth := carbonSummaryApplication(t, cfg, prefix)
					m.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{ge.Provider: {{Name: ge.CarbonModel, Alias: "carbon-friendly"}}})
					var calls atomic.Int32
					ctx := contextWithEnterpriseTransport(t, func(request *http.Request) (*http.Response, error) {
						calls.Add(1)
						wire, _ := io.ReadAll(request.Body)
						if request.Header.Get("Authorization") != "Bearer synthetic-access" || !strings.Contains(request.URL.Path, "/engines/summary-app/") || gjson.GetBytes(wire, "generationSpec.modelId").String() != ge.CarbonModel || gjson.GetBytes(wire, "generationSpec.thinkingConfig.includeThoughts").Type != gjson.True {
							t.Errorf("alias/prefix/pin lost real Carbon summary: path=%s body=%s", request.URL.Path, wire)
						}
						if gjson.GetBytes(wire, "generationSpec.thinkingConfig.thinkingLevel").Exists() || gjson.GetBytes(wire, "generationSpec.thinkingConfig.thinkingBudget").Exists() {
							t.Error("suffix introduced an unverified Carbon level or budget")
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseThoughtAnswer))}, nil
					})
					opts := enterpriseOptions(format)
					opts.Stream = stream
					opts.Metadata[coreexec.PinnedAuthMetadataKey] = auth.ID
					req := coreexec.Request{Model: model, Payload: carbonSummaryRequest(format, "true")}
					var packets [][]byte
					if stream {
						response, err := m.ExecuteStream(ctx, []string{ge.Provider}, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							packets = append(packets, chunk.Payload)
						}
					} else {
						response, err := m.Execute(ctx, []string{ge.Provider}, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						packets = append(packets, response.Payload)
					}
					thought, text, terminals := carbonSummaryText(packets, format, stream)
					if calls.Load() != 1 || thought != "summary 你好🙂summary continued" || text != `{"ok":true}` || terminals != 1 {
						t.Fatalf("alias request lost summary or was replayed: calls=%d thought=%q text=%q terminals=%d", calls.Load(), thought, text, terminals)
					}
				})
			}
		}
	}
}

// Read gates the later upstream frame without relying on timer granularity.
type carbonSummaryGatedBody struct {
	ctx          context.Context
	first, later *strings.Reader
	release      <-chan struct{}
	closed       chan struct{}
	once         sync.Once
}

func (b *carbonSummaryGatedBody) Read(p []byte) (int, error) {
	if b.first != nil {
		n, err := b.first.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != io.EOF {
			return n, err
		}
		b.first = nil
	}
	select {
	case <-b.release:
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
	return b.later.Read(p)
}
func (b *carbonSummaryGatedBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestEnterpriseCarbonSummaryStreamReachesClientBeforeLaterBody(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
		for _, thoughtFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/thoughtFirst=%v", format, thoughtFirst), func(t *testing.T) {
				_, auth := carbonSummaryApplication(t, &config.Config{}, "")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				release := make(chan struct{})
				var released atomic.Bool
				var once sync.Once
				unblock := func() { once.Do(func() { released.Store(true); close(release) }) }
				defer unblock()
				first := `data: {"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"early-summary"}}]}}` + "\n\n"
				later := `data: {"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"later-body"}}]}}` + "\n\n" + `data: {"answer":{"state":"SUCCEEDED"}}` + "\n\n"
				if !thoughtFirst {
					first = `data: {"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"earlier-body"}}]}}` + "\n\n"
					later = `data: {"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"later-summary"}}]}}` + "\n\n" + `data: {"answer":{"state":"SUCCEEDED"}}` + "\n\n"
				}
				body := &carbonSummaryGatedBody{ctx: ctx, first: strings.NewReader(first), later: strings.NewReader(later), release: release, closed: make(chan struct{})}
				defer func() { _ = body.Close() }()
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
				}))
				response, err := NewGeminiEnterpriseExecutor(&config.Config{}).ExecuteStream(ctx, auth, coreexec.Request{Model: ge.CarbonModel, Payload: carbonSummaryRequest(format, "true")}, enterpriseOptions(format))
				if err != nil {
					t.Fatal(err)
				}
				var packets [][]byte
				select {
				case chunk := <-response.Chunks:
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					packets = append(packets, chunk.Payload)
					thought, text, _ := carbonSummaryText(packets, format, true)
					if released.Load() || thoughtFirst && (thought != "early-summary" || text != "") || !thoughtFirst && (text != "earlier-body" || thought != "") {
						t.Fatalf("the first upstream event was buffered/reordered: thought=%q text=%q released=%v", thought, text, released.Load())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("first event waited for a later upstream frame")
				}
				unblock()
				for chunk := range response.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					packets = append(packets, chunk.Payload)
				}
				thought, text, terminal := carbonSummaryText(packets, format, true)
				if thoughtFirst && (thought != "early-summary" || text != "later-body") || !thoughtFirst && (thought != "later-summary" || text != "earlier-body") || terminal != 1 {
					t.Fatalf("incorrect complete stream: %q %q %d", thought, text, terminal)
				}
			})
		}
	}
}

func TestEnterpriseCarbonSummaryFailureDoesNotInventSuccessOrReplay(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"parameter-rejected", "truncated", "upstream-error", "uncertain-submit", "http-500", "http-408", "non-2xx-read-error", "pre-answer-read-error"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", format, stream, scenario), func(t *testing.T) {
					cfg := &config.Config{}
					m, _ := carbonSummaryApplication(t, cfg, "")
					_ = carbonSummaryAddApplication(t, m, "", "summary-spare")
					m.SetRetryConfig(3, 0, 3)
					var calls atomic.Int32
					ctx := contextWithEnterpriseTransport(t, func(request *http.Request) (*http.Response, error) {
						calls.Add(1)
						_, _ = io.ReadAll(request.Body)
						coreexec.MarkUpstreamAttempt(request.Context())
						if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
							trace.WroteRequest(httptrace.WroteRequestInfo{})
						}
						switch scenario {
						case "http-500", "http-408":
							status := http.StatusInternalServerError
							if scenario == "http-408" {
								status = http.StatusRequestTimeout
							}
							return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"uncertain completion","status":"INTERNAL"}}`))}, nil
						case "non-2xx-read-error":
							return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))}, nil
						case "pre-answer-read-error":
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))}, nil
						case "parameter-rejected":
							return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":400,"message":"thinkingConfig.includeThoughts is not supported","status":"INVALID_ARGUMENT"}}`))}, nil
						case "truncated":
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"partial-summary"}},{"content":{"text":"partial-body"}}]}}]`))}, nil
						case "upstream-error":
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"partial-summary"}},{"content":{"text":"partial-body"}}]}},{"error":{"code":500,"message":"temporary upstream error"}}]`))}, nil
						default:
							return nil, io.ErrUnexpectedEOF
						}
					})
					opts := enterpriseOptions(format)
					opts.Stream = stream
					// Both eligible applications remain selectable; uncertain commits must not switch.
					req := coreexec.Request{Model: ge.CarbonModel, Payload: carbonSummaryRequest(format, "true")}
					var packets [][]byte
					var err error
					if stream {
						var response *coreexec.StreamResult
						response, err = m.ExecuteStream(ctx, []string{ge.Provider}, req, opts)
						if err == nil {
							for chunk := range response.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								} else {
									packets = append(packets, chunk.Payload)
								}
							}
						}
					} else {
						_, err = m.Execute(ctx, []string{ge.Provider}, req, opts)
					}
					_, _, terminal := carbonSummaryText(packets, format, stream)
					if err == nil || calls.Load() != 1 || terminal != 0 {
						t.Fatalf("failure was replayed or reported successful: calls=%d terminal=%d err=%v", calls.Load(), terminal, err)
					}
				})
			}
		}
	}
}

func TestEnterpriseCarbonSummaryCancelledStreamClosesWithoutSuccess(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
		t.Run(format.String(), func(t *testing.T) {
			_, auth := carbonSummaryApplication(t, &config.Config{}, "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			gate := make(chan struct{})
			body := &carbonSummaryGatedBody{ctx: ctx, first: strings.NewReader(`data: {"answer":{"state":"IN_PROGRESS","replies":[{"content":{"thought":true,"text":"before-cancel"}}]}}` + "\n\n"), later: strings.NewReader(""), release: gate, closed: make(chan struct{})}
			defer func() { _ = body.Close() }()
			var calls atomic.Int32
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}, nil
			}))
			response, err := NewGeminiEnterpriseExecutor(&config.Config{}).ExecuteStream(ctx, auth, coreexec.Request{Model: ge.CarbonModel, Payload: carbonSummaryRequest(format, "true")}, enterpriseOptions(format))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case chunk := <-response.Chunks:
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				if !strings.Contains(string(chunk.Payload), "before-cancel") {
					t.Fatal("lost visible summary before cancel")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing first summary")
			}
			cancel()
			done := make(chan struct{})
			var packets [][]byte
			go func() {
				defer close(done)
				for chunk := range response.Chunks {
					if chunk.Err == nil {
						packets = append(packets, chunk.Payload)
					}
				}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled stream did not release")
			}
			_, _, terminal := carbonSummaryText(packets, format, true)
			if calls.Load() != 1 || terminal != 0 {
				t.Fatal("cancel replayed generation or emitted success")
			}
		})
	}
}

func TestEnterpriseCarbonSummarySafetyKeepsOrdinaryEnterpriseRetryBehavior(t *testing.T) {
	for _, model := range []string{ge.ArgonModel, "gemini-3.8-flash"} {
		for _, format := range []translator.Format{translator.FormatGemini, translator.FormatOpenAI} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", model, format, stream), func(t *testing.T) {
					cfg := &config.Config{}
					manager := coreauth.NewManager(nil, nil, nil)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(3, 0, 3)
					manager.RegisterExecutor(NewGeminiEnterpriseExecutor(cfg))
					for _, engine := range []string{"ordinary-app", "ordinary-spare"} {
						binding := config.GeminiEnterpriseOAuth{ClientID: "ordinary-summary-client", ProjectID: "123", EngineID: engine}.WithDefaults()
						id := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + engine
						auth := &coreauth.Auth{ID: id, Provider: ge.Provider, Metadata: ge.Metadata(binding, engine+"@example.test", &oauth2.Token{AccessToken: "ordinary-synthetic", RefreshToken: "ordinary-refresh", Expiry: time.Now().Add(time.Hour)}), EnterpriseQuotaIdentity: &ge.QuotaIdentity{Verified: true, Binding: ge.Binding(binding), ProjectNumber: "123", Location: binding.Location, Edition: "enterprise_standard", PrincipalHash: strings.Repeat("b", 64), CheckedAt: time.Now()}}
						auth.Metadata["model_policy"] = "catalog"
						registered, err := manager.Register(t.Context(), auth)
						if err != nil {
							t.Fatal(err)
						}
						trial := true
						flight := manager.BeginEnterpriseSubscriptionRefresh(registered)
						if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_standard", FreeTrial: &trial, State: "ACTIVE", CheckedAt: time.Now()}) {
							t.Fatal("ordinary assigned subscription was not verified")
						}
						manager.PublishEnterprisePolicyCatalog(registered, []ge.Model{{ID: model}})
						registry.GetGlobalRegistry().RegisterClient(registered.ID, ge.Provider, []*registry.ModelInfo{{ID: model, Type: "gemini"}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(registered.ID) })
					}
					var calls atomic.Int32
					ctx := contextWithEnterpriseTransport(t, func(request *http.Request) (*http.Response, error) {
						calls.Add(1)
						_, _ = io.ReadAll(request.Body)
						coreexec.MarkUpstreamAttempt(request.Context())
						if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
							trace.WroteRequest(httptrace.WroteRequestInfo{})
						}
						return nil, io.ErrUnexpectedEOF
					})
					opts := enterpriseOptions(format)
					opts.Stream = stream
					req := coreexec.Request{Model: model, Payload: carbonSummaryRequest(format, "true")}
					var err error
					if stream {
						_, err = manager.ExecuteStream(ctx, []string{ge.Provider}, req, opts)
					} else {
						_, err = manager.Execute(ctx, []string{ge.Provider}, req, opts)
					}
					if err == nil || calls.Load() != 2 {
						t.Fatalf("ordinary retry behavior changed: calls=%d err=%v", calls.Load(), err)
					}
				})
			}
		}
	}
}
