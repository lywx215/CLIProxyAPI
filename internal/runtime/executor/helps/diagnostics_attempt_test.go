package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestDIAG05ModelAttemptProjectionAndAuxiliaryRedirects(t *testing.T) {
	var records []diagnostics.Record
	engine := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, nil, func(b []byte) error {
		var r diagnostics.Record
		if err := json.Unmarshal(b[6:], &r); err != nil {
			return err
		}
		if r.Event == "diag.call" {
			records = append(records, r)
		}
		return nil
	})
	ctx, span := engine.StartServer(context.Background(), nil, "fixture")
	ctx = diagnostics.ExecutorAttempt(ctx, "antigravity")
	sends := 0
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", diagnosticRT(func(r *http.Request) (*http.Response, error) {
		sends++
		header, status := make(http.Header), http.StatusOK
		if r.URL.Path == "/start" {
			header.Set("Location", "/finish")
			status = http.StatusFound
		}
		if r.URL.Host == "vertexaisearch.cloud.google.com" {
			if r.Method != http.MethodHead {
				t.Error("grounding method changed")
			}
			header.Set("Location", "https://fixture.invalid/resolved")
			status = http.StatusFound
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("fixture-body"))}, nil
	}))
	for _, kind := range []string{"model", "other", "auth", "metadata"} {
		for _, redirect := range []bool{false, true} {
			before := len(records)
			requestCtx := cliproxyexecutor.WithUpstreamAttemptTracker(ctx)
			if kind == "auth" || kind == "metadata" {
				requestCtx = diagnostics.WithCallKind(requestCtx, kind)
			}
			client := NewProxyAwareHTTPClient(requestCtx, nil, nil, 0)
			if kind == "model" {
				client = NewUsageReporter(requestCtx, "synthetic", "synthetic", nil).TrackHTTPClient(client)
			}
			path := "/finish"
			if redirect {
				path = "/start"
			}
			req, _ := http.NewRequestWithContext(requestCtx, "GET", "http://fixture.invalid"+path, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, errRead := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if errRead != nil || string(body) != "fixture-body" || resp.StatusCode != 200 {
				t.Fatal("business response changed")
			}
			if cliproxyexecutor.UpstreamAttempted(requestCtx) != (kind == "model") {
				t.Fatal("usage tracker changed")
			}
			wantCalls := 1
			if redirect {
				wantCalls = 2
			}
			if len(records)-before != wantCalls {
				t.Fatal("send count changed")
			}
			for i, r := range records[before:] {
				wantKind := kind
				if i == 1 {
					wantKind = "redirect"
				}
				if r.Data.(map[string]any)["callKind"] != wantKind {
					t.Error("call kind changed")
				}
				if kind == "model" {
					if r.AttemptID == nil || r.AttemptNo == nil || *r.AttemptNo != 1 || r.RetryScope == nil {
						t.Errorf("model/redirect lost actual owner: %+v", r)
					}
				} else if r.AttemptID != nil || r.AttemptNo != nil || r.RetryScope != nil {
					t.Errorf("%s redirect=%t hop=%d inherited model attempt without model label", kind, redirect, i)
				}
			}
		}
	}
	before := len(records)
	input := []byte(`{"response":{"candidates":[{"groundingMetadata":{"groundingChunks":[{"web":{"uri":"https://vertexaisearch.cloud.google.com/grounding-api-redirect/fixture"}}]}}]}}`)
	output := ResolveAntigravityGroundingURLs(ctx, nil, nil, input)
	if gjson.GetBytes(output, "response.candidates.0.groundingMetadata.groundingChunks.0.web.uri").String() != "https://fixture.invalid/resolved" {
		t.Fatal("grounding output changed")
	}
	if len(records) != before+1 {
		t.Fatal("grounding followed an extra redirect")
	}
	r := records[len(records)-1]
	if r.Data.(map[string]any)["callKind"] != "other" || r.AttemptID != nil {
		t.Error("real grounding helper inherited model attempt")
	}
	for i, r := range records {
		if r.CallNo == nil || *r.CallNo != uint64(i+1) {
			t.Fatal("call sequence changed")
		}
	}
	if sends != len(records) {
		t.Fatal("call count differs from actual sends")
	}
	span.Finish(diagnostics.ServerData{EndReason: "finished", DeliveryState: "unknown"})
}
