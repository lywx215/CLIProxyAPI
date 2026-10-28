package geminienterprise

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func modelAvailabilityErrorFixture(code int, status, reason, domain string) []byte {
	data, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": code, "status": status,
		"message": "Model is disabled; sensitive assistToken=fixture-secret",
		"details": []any{map[string]any{
			"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason, "domain": domain,
		}},
	}})
	return data
}

func TestEnterpriseModelNotEnabledStructuredClassification(t *testing.T) {
	now := time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC)
	binding := config.GeminiEnterpriseOAuth{ProjectID: "12345", Location: "global", EngineID: "test-engine"}
	fixture := modelAvailabilityErrorFixture(400, "INVALID_ARGUMENT", "MODEL_NOT_ENABLED", "discoveryengine.googleapis.com")
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(fixture, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		raw    []byte
		status int
		want   bool
	}{
		{"http400_object", fixture, 400, true},
		{"http400_array", append(append([]byte("["), fixture...), ']'), 400, true},
		{"stream200", fixture, 200, true},
		{"stream_rpc_status", envelope.Error, 0, true},
		{"grpc_code", modelAvailabilityErrorFixture(3, "INVALID_ARGUMENT", "MODEL_NOT_ENABLED", "discoveryengine.googleapis.com"), 200, true},
		{"wrong_http_status", fixture, 403, false},
		{"wrong_rpc_status", modelAvailabilityErrorFixture(403, "PERMISSION_DENIED", "MODEL_NOT_ENABLED", "discoveryengine.googleapis.com"), 400, false},
		{"wrong_reason", modelAvailabilityErrorFixture(400, "INVALID_ARGUMENT", "MODEL_DISABLED", "discoveryengine.googleapis.com"), 400, false},
		{"wrong_domain", modelAvailabilityErrorFixture(400, "INVALID_ARGUMENT", "MODEL_NOT_ENABLED", "googleapis.com"), 400, false},
		{"foreign_domain", modelAvailabilityErrorFixture(400, "INVALID_ARGUMENT", "MODEL_NOT_ENABLED", "evil.example"), 400, false},
		{"message_only", []byte(`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"MODEL_NOT_ENABLED discoveryengine.googleapis.com"}}`), 400, false},
		{"metadata_only", []byte(`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","metadata":{"reason":"MODEL_NOT_ENABLED"}}]}}`), 400, false},
		{"wrong_detail_type", []byte(`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.DebugInfo","domain":"discoveryengine.googleapis.com","reason":"MODEL_NOT_ENABLED"}]}}`), 400, false},
		{"conflicting_detail", []byte(`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","reason":"MODEL_NOT_ENABLED"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","reason":"INVALID_QUERY"}]}}`), 400, false},
		{"missing_rpc_status", []byte(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"discoveryengine.googleapis.com","reason":"MODEL_NOT_ENABLED"}]}}`), 400, false},
		{"empty_array", []byte(`[]`), 400, false},
		{"ambiguous_array", []byte("[" + string(fixture) + `,{"error":{"code":400,"status":"INVALID_ARGUMENT"}}]`), 400, false},
		{"malformed", []byte(`{"error":`), 400, false},
		{"trailing_data", append(append([]byte(nil), fixture...), []byte(` {}`)...), 400, false},
		{"oversized", []byte(strings.Repeat(" ", 1024*1024) + string(fixture)), 400, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, started := range []bool{false, true} {
				err := ParseGenerationError(test.raw, test.status, http.Header{"Retry-After": {"7200"}}, binding, nil, now, started)
				if err.ModelNotEnabled != test.want {
					t.Fatalf("started=%v classification=%v want=%v", started, err.ModelNotEnabled, test.want)
				}
				wantScoped := started || !test.want && err.StatusCode() == 400
				if err.Started != started || err.IsRequestScoped() != wantScoped {
					t.Fatal("generation safety boundary changed during classification")
				}
				if err.Restriction != nil {
					t.Fatal("model availability manufactured a quota restriction")
				}
				if test.want && (err.StatusCode() != 400 || err.RetryAfter() != nil) {
					t.Fatal("model availability adopted an upstream quota retry hint")
				}
				if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "assistToken") {
					t.Fatal("structured rejection leaked upstream content")
				}
			}
		})
	}
}

func TestEnterpriseApplicationBindingUsesOnlyCanonicalResources(t *testing.T) {
	base := config.GeminiEnterpriseOAuth{ProjectID: "12345", EngineID: "engine-a"}
	explicit := base.WithDefaults()
	explicit.ClientID = "different-oauth-client"
	explicit.ClientSecretEnv = "DIFFERENT_SECRET_NAME"
	if ApplicationBinding(base) != ApplicationBinding(explicit) {
		t.Fatal("resource-equivalent applications were split by defaults or OAuth configuration")
	}
	for _, change := range []func(*config.GeminiEnterpriseOAuth){
		func(c *config.GeminiEnterpriseOAuth) { c.ProjectID = "54321" },
		func(c *config.GeminiEnterpriseOAuth) { c.Location = "us" },
		func(c *config.GeminiEnterpriseOAuth) { c.CollectionID = "another-collection" },
		func(c *config.GeminiEnterpriseOAuth) { c.EngineID = "another-engine" },
		func(c *config.GeminiEnterpriseOAuth) { c.AssistantID = "another-assistant" },
	} {
		changed := base
		change(&changed)
		if ApplicationBinding(base) == ApplicationBinding(changed) {
			t.Fatal("distinct application resources shared a model availability key")
		}
	}
}
