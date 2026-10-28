package executor

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"golang.org/x/oauth2"
)

func TestEnterpriseCustomFallbackMediaGenerationAndDownload(t *testing.T) {
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"image", "video"} {
		for _, stream := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/stream", false: "/full"}[stream], func(t *testing.T) {
				m := coreauth.NewManager(nil, nil, nil)
				metadata := ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "123", EngineID: "media-app"}, "media@example.test", &oauth2.Token{AccessToken: "synthetic-media", RefreshToken: "synthetic-media-refresh", Expiry: time.Now().Add(time.Hour)})
				metadata["catalog_fallback"] = map[string]any{"mode": "custom", "models": []string{"gemini-3-pro-image", "veo-3.1"}}
				a, err := m.Register(t.Context(), &coreauth.Auth{ID: "media-fallback", Provider: ge.Provider, Metadata: metadata})
				if err != nil {
					t.Fatal(err)
				}
				trial := false
				flight := m.BeginEnterpriseSubscriptionRefresh(a)
				if !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial}) {
					t.Fatal("paid subscription fixture failed")
				}
				m.PublishEnterpriseCatalogState(a, ge.CatalogState{Source: "none", Status: "unavailable", LastError: &ge.MetadataError{Code: "catalog_unavailable"}})
				mime, data, model := "image/png", pngData.Bytes(), "gemini-3-pro-image"
				if kind == "video" {
					mime, data, model = "video/mp4", []byte("\x00\x00\x00\x18ftypisomtest"), "veo-3.1"
				}
				generated, downloaded := 0, 0
				engine := ge.Engine(ge.FromMetadata(a.Metadata))
				ctx := contextWithEnterpriseTransport(t, func(req *http.Request) (*http.Response, error) {
					if req.Header.Get("Authorization") != "Bearer synthetic-media" {
						t.Fatal("media credential changed")
					}
					if req.Method == http.MethodPost {
						generated++
						reply := fmt.Sprintf(`[{"sessionInfo":{"session":%q},"assistToken":"synthetic-assist","answer":{"state":"IN_PROGRESS","replies":[{"content":{"file":{"fileId":"file-1","mimeType":%q}}}]}},{"answer":{"state":"SUCCEEDED"}}]`, engine+"/sessions/fixture", mime)
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(reply))}, nil
					}
					downloaded++
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(base64.StdEncoding.EncodeToString(data)))}, nil
				})
				cfg := &config.Config{}
				e := NewGeminiEnterpriseExecutor(cfg)
				request := coreexec.Request{Model: model, Payload: []byte(`{"messages":[{"role":"user","content":"A test scene."}]}`)}
				output := []byte{}
				if stream {
					result, err := e.ExecuteStream(ctx, a, request, enterpriseOptions(translator.FormatOpenAI))
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output = append(output, chunk.Payload...)
					}
				} else {
					result, err := e.Execute(ctx, a, request, enterpriseOptions(translator.FormatOpenAI))
					if err != nil {
						t.Fatal(err)
					}
					output = result.Payload
				}
				if generated != 1 || downloaded != 1 || !bytes.Contains(output, []byte(base64.StdEncoding.EncodeToString(data))) {
					t.Fatalf("generation=%d download=%d output=%s", generated, downloaded, output)
				}
				if kind == "image" {
					cfg.DisableImageGeneration = config.DisableImageGenerationAll
					generated = 0
					_, err = e.Execute(ctx, a, request, enterpriseOptions(translator.FormatOpenAI))
					if err == nil || generated != 0 {
						t.Fatal("local media disablement bypassed")
					}
				}
			})
		}
	}
}

func TestEnterpriseFallbackFinalBarrierDispatchAndUsage(t *testing.T) {
	for _, region := range []string{"global", "us", "eu", "ca", "au", "in"} {
		for _, stream := range []bool{false, true} {
			for _, paid := range []bool{false, true} {
				t.Run(region+map[bool]string{true: "/paid", false: "/trial"}[paid]+map[bool]string{true: "/stream", false: "/full"}[stream], func(t *testing.T) {
					m := coreauth.NewManager(nil, nil, nil)
					a := &coreauth.Auth{ID: "fallback-executor", Provider: ge.Provider, Metadata: ge.Metadata(config.GeminiEnterpriseOAuth{ClientID: "fixture", ProjectID: "123", Location: region, EngineID: "fixture-app"}, "fixture@example.test", &oauth2.Token{AccessToken: "synthetic", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)})}
					var err error
					a, err = m.Register(t.Context(), a)
					if err != nil {
						t.Fatal(err)
					}
					trial := !paid
					flight := m.BeginEnterpriseSubscriptionRefresh(a)
					if flight == nil || !flight.Complete(ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial}) {
						t.Fatal("subscription fixture failed")
					}
					m.PublishEnterpriseCatalogState(a, ge.CatalogState{Source: "none", Status: "unavailable", LastError: &ge.MetadataError{Code: "service_disabled"}})
					model := "gemini-3.8-flash"
					if paid {
						model = ge.ArgonModel
					}
					calls := 0
					ctx := contextWithEnterpriseTransport(t, func(req *http.Request) (*http.Response, error) {
						calls++
						if req.URL.Host != strings.TrimPrefix(ge.OriginForLocation(region), "https://") {
							t.Fatal("wrong regional endpoint")
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(enterpriseAnswer))}, nil
					})
					cfg := &config.Config{}
					e := NewGeminiEnterpriseExecutor(cfg)
					req := coreexec.Request{Model: model, Payload: []byte(`{"contents":[{"parts":[{"text":"Explain a short test."}]}]}`)}
					output := ""
					if stream {
						result, err := e.ExecuteStream(ctx, a, req, enterpriseOptions(translator.FormatGemini))
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							output += string(chunk.Payload)
						}
					} else {
						result, err := e.Execute(ctx, a, req, enterpriseOptions(translator.FormatGemini))
						if err != nil {
							t.Fatal(err)
						}
						output = string(result.Payload)
					}
					if calls != 1 || !strings.Contains(output, "hello") || !strings.Contains(output, "usageMetadata") {
						t.Fatalf("calls=%d output=%s", calls, output)
					}
					// Override the final model to one forbidden by either trial or paid policy.
					forbidden := ge.ArgonModel
					if paid {
						forbidden = "gemini-3.8-flash"
					}
					cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: ge.Provider}}, Params: map[string]any{"generationSpec.modelId": forbidden}}}
					calls = 0
					if stream {
						_, err = e.ExecuteStream(ctx, a, req, enterpriseOptions(translator.FormatGemini))
					} else {
						_, err = e.Execute(ctx, a, req, enterpriseOptions(translator.FormatGemini))
					}
					var rejection *coreauth.EnterpriseModelPolicyError
					if calls != 0 || !errors.As(err, &rejection) {
						t.Fatalf("forbidden fallback model dispatched: calls=%d err=%v", calls, err)
					}
					current, _ := m.GetByID(a.ID)
					if current.Quota.Exceeded || !current.NextRetryAfter.IsZero() {
						t.Fatal("local rejection cooled credential")
					}
				})
			}
		}
	}
}
