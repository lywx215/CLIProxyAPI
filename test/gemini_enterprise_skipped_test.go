package test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestEnterprisePublicSkippedStopsRetriesAndReturnsReasons(t *testing.T) {
	for _, protocol := range []string{"openai", "gemini"} {
		for _, stream := range []bool{false, true} {
			for _, partial := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/partial=%v", protocol, stream, partial), func(t *testing.T) {
					cfg := &config.Config{
						SDKConfig: config.SDKConfig{Streaming: config.StreamingConfig{BootstrapRetries: 3}},
						OAuthRequestScopedErrors: map[string][]config.RequestScopedErrorRule{
							ge.Provider: {{Status: 400, Match: []string{"upstream_skipped"}, Action: coreauth.RequestScopedActionContinueAndCooldown}},
						},
					}
					router, transport, manager := enterpriseFixtureRouterWithConfig(t, cfg)
					manager.SetConfig(cfg)
					manager.SetRetryConfig(3, 0, 3)
					transport.upstreamStatus = 200
					transport.upstreamBody = `{"answer":{"state":"SKIPPED","assistSkippedReasons":["NON_ASSIST_SEEKING_QUERY_IGNORED"],"customerPolicyEnforcementResult":{"private":"synthetic-private-policy"}},"assistToken":"synthetic-private-token"}`
					if partial {
						transport.upstreamBody = `{"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"partial answer"}}]}}` + transport.upstreamBody
					}
					path, body := enterprisePublicRequest(t, "gemini-3.8-flash", protocol, stream, nil)
					response := enterprisePublicCall(router, path, body)
					wantStatus := http.StatusBadRequest
					if stream && partial {
						wantStatus = http.StatusOK
					}
					if response.Code != wantStatus || transport.calls.Load() != 1 {
						t.Fatalf("skip was replayed or misclassified: status=%d calls=%d body=%s", response.Code, transport.calls.Load(), response.Body.String())
					}
					if gjson.GetBytes(transport.requestBody(), "assistSkippingMode").String() != "REQUEST_ASSIST" {
						t.Fatal("public request did not ask Google to assist by default")
					}
					packets := []string{response.Body.String()}
					if stream && partial {
						packets = nil
						for _, line := range strings.Split(response.Body.String(), "\n") {
							if strings.HasPrefix(line, "data:") {
								packets = append(packets, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
							}
						}
					}
					failures := 0
					for _, packet := range packets {
						if gjson.Get(packet, "error.code").String() == "upstream_skipped" {
							failures++
							if gjson.Get(packet, "error.skip_reasons.0").String() != "NON_ASSIST_SEEKING_QUERY_IGNORED" || gjson.Get(packet, "error.retryable").Type != gjson.False || !strings.Contains(gjson.Get(packet, "error.message").String(), "NON_ASSIST_SEEKING_QUERY_IGNORED") {
								t.Fatalf("skip details were lost: %s", packet)
							}
						}
					}
					if failures != 1 || strings.Contains(response.Body.String(), "synthetic-private") {
						t.Fatalf("invalid public skip response: %s", response.Body.String())
					}
					current, _ := manager.GetByID("enterprise-wire-fixture")
					if current.Unavailable || current.LastError != nil || len(current.ModelStates) != 0 {
						t.Fatal("skip rejection penalized the credential")
					}
				})
			}
		}
	}
}
