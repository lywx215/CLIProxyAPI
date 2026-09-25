package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type semanticRT func(*http.Request) (*http.Response, error)

func (f semanticRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type semanticBody struct {
	chunks [][]byte
	reads  atomic.Int64
	end    error
	closed atomic.Bool
}

func (b *semanticBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	if len(b.chunks) == 0 {
		return 0, b.end
	}
	n := copy(p, b.chunks[0])
	b.chunks[0] = b.chunks[0][n:]
	if len(b.chunks[0]) == 0 {
		b.chunks = b.chunks[1:]
	}
	return n, nil
}
func (b *semanticBody) Close() error {
	b.closed.Store(true)
	return errors.New("private cleanup failure")
}

func TestDIAG05StreamBackpressureCancellationAndTailUsage(t *testing.T) {
	for _, mode := range []string{"tail", "read-error", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var baseline [][]byte
				for _, observed := range []bool{false, true} {
					ctx, cancel := context.WithCancel(context.Background())
					body := &semanticBody{end: io.EOF, chunks: [][]byte{
						[]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"first\"}]}}]}\n\n"),
						[]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"last\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":7,\"thoughtsTokenCount\":80,\"totalTokenCount\":92}}\n\n"),
					}}
					if mode == "read-error" {
						body.chunks = body.chunks[:1]
						body.end = errors.New("private read failure")
					}
					var mu sync.Mutex
					var records []diagnostics.Record
					e := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, func() bool { return true }, func(b []byte) error {
						var r diagnostics.Record
						if err := json.Unmarshal(b[6:], &r); err != nil {
							return err
						}
						mu.Lock()
						records = append(records, r)
						mu.Unlock()
						return nil
					})
					var span *diagnostics.ServerSpan
					if observed {
						ctx, span = e.StartServer(ctx, nil, "fixture")
					}
					ctx = context.WithValue(ctx, "cliproxy.roundtripper", semanticRT(func(*http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
					}))
					result, err := NewGeminiExecutor(&config.Config{}).ExecuteStream(ctx, &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "http://fixture.invalid"}}, cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, Stream: true})
					if err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					if body.reads.Load() != 1 {
						t.Fatalf("observer pre-read across blocked send: %d", body.reads.Load())
					}
					if mode == "cancelled" {
						cancel()
						synctest.Wait()
					}
					var got [][]byte
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							got = append(got, []byte("error"))
						} else {
							got = append(got, chunk.Payload)
						}
					}
					if !body.closed.Load() {
						t.Fatal("cleanup lost")
					}
					if observed {
						span.Finish(diagnostics.ServerData{EndReason: "finished", DeliveryState: "unknown"})
						if !equalPayloads(got, baseline) {
							t.Fatal("send order/payload/error changed")
						}
						mu.Lock()
						found := false
						for _, r := range records {
							if r.Event != "upstream.attempt_finished" {
								continue
							}
							found = true
							d := r.Data.(map[string]any)
							want := "success"
							if mode == "read-error" {
								want = "error"
							}
							if mode == "cancelled" {
								want = "cancelled"
							}
							if d["resultClass"] != want {
								t.Fatalf("terminal %v", d)
							}
							if mode == "tail" && d["usage"].(map[string]any)["outputTotal"].(map[string]any)["value"] != float64(87) {
								t.Fatal("tail usage lost")
							}
						}
						mu.Unlock()
						if !found {
							t.Fatal("semantic terminal missing")
						}
					} else {
						baseline = got
					}
					cancel()
				}
			})
		})
	}
}
