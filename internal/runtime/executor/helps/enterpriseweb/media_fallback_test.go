package enterpriseweb

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

func TestMediaFallbackProjectionKeepsHistoryAndCurrentInput(t *testing.T) {
	payload := []byte(`{
		"systemInstruction":{"parts":[{"text":"system"}]},
		"contents":[
			{"role":"user","parts":[{"text":"first","inline_data":{"data":"private-inline","mimeType":"image/png"},"mediaResolution":{}}]},
			{"role":"model","parts":[{"text":"response"},{"file_data":{"file_uri":"https://fixture.test/private"}}]},
			{"role":"model","parts":[{"inlineData":{"data":"private-model"}}]},
			{"parts":[{"functionResponse":{"name":"tool","response":{"inlineData":"tool result"}}},{"inlineData":{"data":"private-latest"}}]}
		],
		"generationConfig":{"thinkingConfig":{"includeThoughts":true}}
	}`)
	r := NewRuntime()
	ctx, clean, err := r.ProjectMediaFallback(context.Background(), payload, FallbackWebDisabled)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := MediaFallbackFrom(ctx)
	if !ok || info.HeaderValue() != "web_disabled/current_input" || info.RequestBytes != int64(len(payload)) || info.RemovedAttachments != 4 {
		t.Fatalf("projection marker = %+v", info)
	}
	if HasMediaInContext(ctx, clean) || bytes.Contains(clean, []byte("private")) || !gjson.ValidBytes(clean) {
		t.Fatal("projection retained media or produced invalid JSON")
	}
	for path, want := range map[string]string{
		"contents.#": "3", "contents.0.parts.0.text": "first", "contents.1.parts.0.text": "response",
		"contents.2.parts.0.functionResponse.response.inlineData": "tool result",
		"systemInstruction.parts.0.text":                          "system", "generationConfig.thinkingConfig.includeThoughts": "true",
	} {
		if got := gjson.GetBytes(clean, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if r.Status()["inflight_bytes"] != int64(0) {
		t.Fatal("projection retained its scratch budget")
	}
}

func TestMediaFallbackProjectionReusesNearestUserTurn(t *testing.T) {
	body := []byte(`{"contents":[
		{"parts":[{"text":"oldest"}]},
		{"role":"model","parts":[{"text":"old response"}]},
		{"parts":[{"text":"near one"}]},
		{"role":"u\u0073er","parts":[{"text":"near two","inli\u006eeData":{"data":"discard"}},{"text":"\u2003"}]},
		{"role":"model","parts":[{"text":"hidden","thought":true},{"text":"visible reply"}]},
		{"parts":[{"fileData":{"fileUri":"files/current"}}]},
		{"role":"model","parts":[{"text":"continuation"}]}
	]}`)
	ctx, clean, err := NewRuntime().ProjectMediaFallback(context.Background(), body, FallbackNoCredential)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := MediaFallbackFrom(ctx)
	last := gjson.GetBytes(clean, "contents.6")
	if info.HeaderValue() != "no_web_credential/history_text" || info.RemovedAttachments != 2 ||
		last.Get("role").String() != "user" || last.Get("parts.#").Int() != 2 ||
		last.Get("parts.0.text").String() != "near one" || last.Get("parts.1.text").String() != "near two" {
		t.Fatalf("reused text = %s, marker = %+v", last.Raw, info)
	}
}

func TestMediaFallbackProjectionRejectsInvalidOrMissingUserText(t *testing.T) {
	for _, test := range []struct {
		name, input, reason string
	}{
		{"invalid JSON", `{"contents":[{"parts":[{"inlineData":{"data":"x"}}]}`, "invalid_json"},
		{"invalid outer whitespace", "\u00a0" + `{"contents":[{"parts":[{"text":"a","inlineData":{"data":"x"}}]}]}`, "invalid_json"},
		{"duplicate parts", `{"contents":[{"parts":[{"inlineData":{"data":"x"}}],"parts":[{"text":"duplicate"}]}]}`, "invalid_history"},
		{"duplicate escaped key", `{"contents":[{"parts":[{"text":"a","t\u0065xt":"b","inlineData":{"data":"x"}}]}]}`, "invalid_history"},
		{"model text only", `{"contents":[{"role":"model","parts":[{"text":"not user input"}]},{"parts":[{"inlineData":{"data":"x"}}]}]}`, "media_fallback_no_text"},
		{"unicode spaces", `{"contents":[{"parts":[{"text":"\u2003\t"},{"inlineData":{"data":"x"}}]}]}`, "media_fallback_no_text"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := NewRuntime()
			_, clean, err := r.ProjectMediaFallback(context.Background(), []byte(test.input), FallbackWebDisabled)
			_, reason := PublicErrorDetail(err, 400)
			if code(err) != 400 || reason != test.reason || clean != nil || r.Status()["inflight_bytes"] != int64(0) {
				t.Fatalf("invalid projection = %v, reason = %q, status = %v", err, reason, r.Status())
			}
		})
	}
}

func TestReadGenerationMediaFallbackPreservesResourceIsolation(t *testing.T) {
	for _, known := range []bool{false, true} {
		r := NewRuntime()
		r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, MaxConcurrent: 1})
		_, webLease, err := r.Acquire(context.Background(), 1024)
		if err != nil {
			t.Fatal(err)
		}
		// The late first media key exercises the existing prefix spool.
		prefix := strings.Repeat("a", 70_000)
		wire := `{"contents":[{"parts":[{"text":"` + prefix + `"},{"inlineData":{"data":"` + strings.Repeat("x", 110_000) + `"}}]}]}`
		length := int64(-1)
		if known {
			length = int64(len(wire))
		}
		authorized, selections := false, 0
		clean, ctx, lease, err := ReadGenerationBody(context.Background(), fragmentReader{strings.NewReader(wire), 1013}, length, r, func() error {
			authorized = true
			return nil
		}, func() (bool, bool) {
			if !authorized {
				t.Error("fallback checked credential availability before request authentication")
			}
			selections++
			return true, false
		})
		if err != nil || lease != nil || selections != 1 || gjson.GetBytes(clean, "contents.0.parts.0.text").Str != prefix {
			t.Fatalf("known=%t: unexpected projection %v / %v / %d", known, err, lease, selections)
		}
		if fallback, ok := MediaFallbackFrom(ctx); !ok || fallback.HeaderValue() != "no_web_credential/current_input" || fallback.RequestBytes != int64(len(wire)) {
			t.Fatalf("known=%t: marker %+v", known, fallback)
		}
		if state := r.Status(); state["active_requests"] != 1 || state["inflight_bytes"] != webLease.Budget() || state["rejected_concurrency"] != uint64(0) {
			t.Fatalf("fallback consumed a Web slot or retained scratch: %v", state)
		}
		webLease.Release()
	}
}

func TestMediaFallbackLeaseTransferRetainsOriginalBuffer(t *testing.T) {
	const raw = `{"contents":[{"parts":[{"text":"question"},{"inlineData":{"data":"discard"}}]}]}`
	r := enabledRuntime()
	body := []byte(raw)
	ctx, lease, err := r.Acquire(context.Background(), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	replacement := bytes.Clone(body)
	projected, clean, err := r.ProjectMediaFallback(WithBody(ctx, body), replacement, FallbackNoCredential)
	if err != nil || ExistingLease(projected) != nil || HasMedia(clean) {
		t.Fatalf("lease transfer failed: %v", err)
	}
	if state := r.Status(); state["active_requests"] != 0 || state["inflight_bytes"] != int64(cap(body)+cap(replacement)) {
		t.Fatalf("raw bytes released before the original owner ended: %v", state)
	}
	r.RecordMediaFallback(projected)
	if counts := r.Status()["media_fallback_requests"].(map[string]uint64); counts["no_web_credential"] != 1 || counts["web_disabled"] != 0 {
		t.Fatalf("incorrect fallback counts: %v", counts)
	}
	lease.Release()
	lease.Release()
	if state := r.Status(); state["active_requests"] != 0 || state["inflight_bytes"] != int64(0) {
		t.Fatalf("lease release leaked capacity: %v", state)
	}

	ctx, lease, err = r.Acquire(context.Background(), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	lease.MarkSessionStarted()
	if _, _, err := r.ProjectMediaFallback(ctx, body, FallbackNoCredential); code(err) != 503 {
		t.Fatalf("projection allowed after session creation: %v", err)
	}
}

func TestReadGenerationMediaFallbackUsesSwitchAtAdmission(t *testing.T) {
	const body = `{"contents":[{"parts":[{"text":"q"},{"inlineData":{"data":"discard"}}]}]}`
	r := enabledRuntime()
	_, ctx, lease, err := ReadGenerationBody(context.Background(), strings.NewReader(body), int64(len(body)), r, nil, func() (bool, bool) {
		r.Configure(config.GeminiEnterpriseWebConfig{})
		return true, true
	})
	fallback, _ := MediaFallbackFrom(ctx)
	if err != nil || lease != nil || fallback.Reason != FallbackWebDisabled || r.Status()["inflight_bytes"] != int64(0) {
		t.Fatalf("switch raced admission: %v, fallback = %+v", err, fallback)
	}
}

func TestReadGenerationMediaFallbackDoesNotStripImageOutput(t *testing.T) {
	const body = `{"generation_config":{"responseModalities":["IMAGE"]},"contents":[{"parts":[{"text":"q"},{"inlineData":{"data":"discard"}}]}]}`
	r := NewRuntime()
	_, ctx, lease, err := ReadGenerationBody(context.Background(), strings.NewReader(body), int64(len(body)), r, nil, func() (bool, bool) {
		return true, false
	})
	if _, projected := MediaFallbackFrom(ctx); !IsChannelDisabled(err) || projected || lease != nil || r.Status()["inflight_bytes"] != int64(0) {
		t.Fatalf("image output entered text fallback: %v", err)
	}
}

func TestReadGenerationMediaFallbackRespectsLimitsAndCancellation(t *testing.T) {
	const body = `{"contents":[{"parts":[{"text":"q"},{"inlineData":{"data":"discard"}}]}]}`
	eligible := func() (bool, bool) { return true, true }
	for _, test := range []struct {
		name   string
		limit  int64
		budget int64
		status int
	}{
		{"original request cap", 64, 4096, 413},
		{"projection memory", 4096, 128, 429},
		{"closed but sufficient memory", 4096, 4096, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := NewRuntime()
			r.Configure(config.GeminiEnterpriseWebConfig{MaxInflightBytes: test.budget, InputLimits: config.GeminiEnterpriseWebInputLimits{MaxRequestBytes: test.limit}})
			clean, ctx, lease, err := ReadGenerationBody(context.Background(), strings.NewReader(body), int64(len(body)), r, nil, eligible)
			if code(err) != test.status || lease != nil || r.Status()["inflight_bytes"] != int64(0) {
				t.Fatalf("status=%v state=%v", err, r.Status())
			}
			if test.status == 0 {
				info, ok := MediaFallbackFrom(ctx)
				if !ok || info.Reason != FallbackWebDisabled || HasMedia(clean) ||
					r.Status()["preflight_rejections"].(map[string]uint64)["channel_disabled"] != 0 {
					t.Fatalf("closed channel did not project: %+v", info)
				}
			}
		})
	}
	r := NewRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	reader := io.MultiReader(strings.NewReader(body[:len(body)-2]), strings.NewReader(body[len(body)-2:]))
	_, _, _, err := ReadGenerationBody(ctx, reader, -1, r, nil, func() (bool, bool) {
		cancel()
		return true, false
	})
	if err != context.Canceled || r.Status()["inflight_bytes"] != int64(0) {
		t.Fatalf("cancelled projection = %v, state = %v", err, r.Status())
	}
}
