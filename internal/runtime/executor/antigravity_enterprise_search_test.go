package executor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityEnterpriseStandardSearchNativeBoundary(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, alias := range []string{"googleSearch", "google_search"} {
			t.Run(fmt.Sprintf("%s/stream=%v", alias, stream), func(t *testing.T) {
				generations := 0
				ctx := aeMockContext(t, aeQuotaPositive, func(request *http.Request) (*http.Response, error) {
					generations++
					body, _ := io.ReadAll(request.Body)
					if !gjson.GetBytes(body, "tools.0.enterpriseWebSearch").IsObject() || gjson.GetBytes(body, "tools.0.googleSearch").Exists() || gjson.GetBytes(body, "tools.0.google_search").Exists() {
						t.Fatalf("standard search did not reach the native boundary: %s", body)
					}
					if stream {
						return aeResponse(200, "text/event-stream", "data: "+aeAnswer+"\n\n"), nil
					}
					return aeResponse(200, "application/json", aeAnswer), nil
				})
				executor := NewAntigravityEnterpriseExecutor(&config.Config{})
				request := coreexec.Request{Model: "gemini-3.8-flash-low", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"search"}]}],"tools":[{"` + alias + `":{}}]}`)}
				opts := coreexec.Options{SourceFormat: translator.FormatGemini}
				if stream {
					result, err := executor.ExecuteStream(ctx, aeExecutorAuth("global"), request, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(ctx, aeExecutorAuth("global"), request, opts); err != nil {
					t.Fatal(err)
				}
				if generations != 1 {
					t.Fatalf("generation calls = %d, want 1", generations)
				}
			})
		}
	}
}

func TestAntigravityEnterpriseSearchPreservesFinalPayloadRules(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, filter := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/filter=%v", stream, filter), func(t *testing.T) {
				cfg := &config.Config{}
				if filter {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini"}}, Params: []string{"tools"}}}
				} else {
					cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini"}}, Params: map[string]any{"tools.0.enterpriseWebSearch.excludeDomains": []string{"configured.test"}}}}
				}
				ctx := aeMockContext(t, aeQuotaPositive, func(request *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(request.Body)
					if filter {
						if gjson.GetBytes(body, "tools").Exists() {
							t.Fatalf("filtered tools were restored: %s", body)
						}
					} else if gjson.GetBytes(body, "tools.0.enterpriseWebSearch.excludeDomains.0").String() != "configured.test" {
						t.Fatalf("configured search restriction was overwritten: %s", body)
					}
					if stream {
						return aeResponse(200, "text/event-stream", "data: "+aeAnswer+"\n\n"), nil
					}
					return aeResponse(200, "application/json", aeAnswer), nil
				})
				executor := NewAntigravityEnterpriseExecutor(cfg)
				request := coreexec.Request{Model: "gemini-3.8-flash-low", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"search"}]}],"tools":[{"googleSearch":{}}]}`)}
				opts := coreexec.Options{SourceFormat: translator.FormatGemini}
				if stream {
					result, err := executor.ExecuteStream(ctx, aeExecutorAuth("global"), request, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(ctx, aeExecutorAuth("global"), request, opts); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestAntigravityEnterpriseSearchRejectsUnsupportedOptionsBeforeGeneration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			generations := 0
			ctx := aeMockContext(t, aeQuotaPositive, func(*http.Request) (*http.Response, error) {
				generations++
				return aeResponse(200, "application/json", aeAnswer), nil
			})
			executor := NewAntigravityEnterpriseExecutor(&config.Config{})
			request := coreexec.Request{Model: "gemini-3.8-flash-low", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"search"}]}],"tools":[{"googleSearch":{"timeRangeFilter":{"startTime":"2026-01-01T00:00:00Z","endTime":"2026-01-02T00:00:00Z"}}}]}`)}
			opts := coreexec.Options{SourceFormat: translator.FormatGemini}
			var err error
			if stream {
				_, err = executor.ExecuteStream(ctx, aeExecutorAuth("global"), request, opts)
			} else {
				_, err = executor.Execute(ctx, aeExecutorAuth("global"), request, opts)
			}
			var failure *ae.Error
			if !errors.As(err, &failure) || failure.HTTP != 400 || failure.Code != "unsupported_search_options" || !failure.IsRequestScoped() || failure.Attempted || generations != 0 {
				t.Fatalf("expected an unattempted request-scoped 400, got %v; generations=%d", err, generations)
			}
		})
	}
}

func TestAntigravityEnterpriseSearchRulesCannotReintroduceUnmappedAlias(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Protocol: "gemini"}}, Params: map[string]any{"tools.0.googleSearch": map[string]any{}}}}
			generations := 0
			ctx := aeMockContext(t, aeQuotaPositive, func(*http.Request) (*http.Response, error) {
				generations++
				return aeResponse(200, "application/json", aeAnswer), nil
			})
			executor := NewAntigravityEnterpriseExecutor(cfg)
			request := coreexec.Request{Model: "gemini-3.8-flash-low", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"search"}]}],"tools":[{"googleSearch":{}}]}`)}
			opts := coreexec.Options{SourceFormat: translator.FormatGemini}
			var err error
			if stream {
				_, err = executor.ExecuteStream(ctx, aeExecutorAuth("global"), request, opts)
			} else {
				_, err = executor.Execute(ctx, aeExecutorAuth("global"), request, opts)
			}
			var failure *ae.Error
			if !errors.As(err, &failure) || failure.HTTP != 400 || failure.Code != "invalid_search_tools" || !failure.IsRequestScoped() || failure.Attempted || generations != 0 {
				t.Fatalf("final rule result was rewritten or sent upstream: %v; generations=%d", err, generations)
			}
		})
	}
}
