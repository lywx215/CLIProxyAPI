package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type queuedGenerationBody struct {
	gate   <-chan struct{}
	reader *strings.Reader
}

func (b *queuedGenerationBody) Read(data []byte) (int, error) {
	if b.gate != nil {
		<-b.gate
		b.gate = nil
	}
	return b.reader.Read(data)
}
func (b *queuedGenerationBody) Close() error { return nil }

func TestGeminiAdmissionConductorQueuesBeforeTransmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, auths := budgetIntegrationManager(t, 1)
		gate := make(chan struct{})
		firstStarted := make(chan struct{})
		var sends atomic.Int32
		ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", budgetIntegrationTransport(func(req *http.Request) (*http.Response, error) {
			number := sends.Add(1)
			body := &queuedGenerationBody{reader: strings.NewReader(`{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)}
			if number == 1 {
				body.gate = gate
				close(firstStarted)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
		}))
		results := make(chan error, 3)
		invoke := func() {
			_, err := manager.Execute(ctx, []string{"gemini"}, cliproxyexecutor.Request{Model: "public-lite", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"test"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
			results <- err
		}
		go invoke()
		<-firstStarted
		go invoke()
		synctest.Wait()
		go invoke()
		synctest.Wait()
		view := manager.GeminiAdmissionQueueStatus()
		if sends.Load() != 1 || view.Pending != 2 || view.Selecting {
			t.Fatalf("queued requests entered transport early: sends=%d queue=%+v", sends.Load(), view)
		}
		engine := manager.GeminiBudgetEngine()
		scope := coreauth.GeminiBudgetScope(auths[0], "gemini-3.5-flash-lite")
		if value := engine.View(scope); value.Used != 1 || value.InFlight != 1 {
			t.Fatalf("queue reserved budget: %+v", value)
		}
		close(gate)
		for range 3 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		if sends.Load() != 3 {
			t.Fatalf("physical sends=%d", sends.Load())
		}
		if value := engine.View(scope); value.Used != 3 || value.InFlight != 0 {
			t.Fatalf("incorrect final budget: %+v", value)
		}
		if view := manager.GeminiAdmissionQueueStatus(); view.Pending != 0 || view.Selecting || view.Dispatched != 3 {
			t.Fatalf("selection turn leaked: %+v", view)
		}
	})
}

var _ io.ReadCloser = (*queuedGenerationBody)(nil)
