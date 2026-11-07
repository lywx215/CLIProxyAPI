package executor

import (
	"context"
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
)

func TestEnterpriseCarbonTestCapturesPrivateSession(t *testing.T) {
	for _, scenario := range []string{"verified", "ordinary", "conflicting"} {
		t.Run(scenario, func(t *testing.T) {
			paid := false
			auth := enterpriseExecutorSubscribedAuth(t, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &paid, State: "ACTIVE", CheckedAt: time.Now()})
			session := ge.Engine(ge.FromMetadata(auth.Metadata)) + "/sessions/private-test-session"
			wire := `[{"sessionInfo":{"session":"` + session + `"},"answer":{"state":"IN_PROGRESS","replies":[{"content":{"text":"verified"}}]}},{"answer":{"state":"SUCCEEDED"}}]`
			model := ge.CarbonModel
			if scenario == "ordinary" {
				model = ge.ArgonModel
			}
			if scenario == "conflicting" {
				wire = strings.TrimSuffix(wire, "]") + `,{"sessionInfo":{"session":"` + session + `-different"}}]`
			}
			calls := 0
			transport := enterpriseTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
			if scenario != "ordinary" {
				ctx = coreauth.WithEnterpriseCarbonModelTest(ctx, auth.ID, ge.Resources(ge.FromMetadata(auth.Metadata)))
			}
			response, err := NewGeminiEnterpriseExecutor(&config.Config{}).Execute(ctx, auth, coreexec.Request{Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"verification"}]}]}`)}, enterpriseOptions(translator.FormatGemini))
			if scenario == "conflicting" {
				if err == nil {
					t.Fatal("conflicting sessions created a successful proof")
				}
				return
			}
			if err != nil || calls != 1 || strings.Contains(string(response.Payload), "private-test-session") || strings.Contains(string(response.Payload), "sessionInfo") {
				t.Fatalf("unexpected execution or public session leak: err=%v calls=%d", err, calls)
			}
			if scenario == "ordinary" {
				if response.Metadata != nil {
					t.Fatal("ordinary requests received private test metadata")
				}
			} else if response.Metadata["enterprise_carbon_session"] != session || response.Metadata["enterprise_carbon_model"] != ge.CarbonModel {
				t.Fatal("management proof did not capture the exact session and final model")
			}
		})
	}
}
