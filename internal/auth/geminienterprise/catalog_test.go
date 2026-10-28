package geminienterprise

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestEnterpriseCatalogFallbackSettingsAndDefaults(t *testing.T) {
	trial, paid := true, false
	for _, tc := range []struct {
		edition string
		trial   *bool
		status  string
		count   int
		first   string
	}{
		{"enterprise_plus", &trial, "ok", 6, "gemini-3.8-flash"}, {"enterprise_standard", &trial, "stale", 6, "gemini-3.8-flash"},
		{"business_standard", &trial, "ok", 1, ArgonModel}, {"enterprise_plus", &paid, "ok", 1, ArgonModel}, {"business_standard", &paid, "stale", 1, ArgonModel},
		{"enterprise_plus", nil, "unknown", 0, ""}, {"enterprise_plus", &trial, "expired", 0, ""},
	} {
		models, _ := FallbackCatalog(CatalogFallback{Mode: "default"}, SubscriptionObservation{Edition: tc.edition, FreeTrial: tc.trial, Status: tc.status})
		if len(models) != tc.count || tc.count > 0 && models[0].ID != tc.first {
			t.Fatalf("%+v: %v", tc, models)
		}
	}
	for _, raw := range []string{`{"mode":"custom","models":["gemini-3.*"]}`, `{"mode":"custom","models":["work/gemini-3.8-flash"]}`, `{"mode":"custom","models":["auto"]}`, `{"mode":"custom","models":["gemini-3.8-flash(high)"]}`, `{"mode":"custom","models":["veo-3.1-fast"]}`, `{"mode":"custom","models":["gemini-3-pro-image-preview"]}`, `{"mode":"unknown"}`, `{"mode":"custom","models":"gemini-3.8-flash"}`, `{"mode":"custom","extra":true}`} {
		var value any
		_ = json.Unmarshal([]byte(raw), &value)
		if _, err := CatalogFallbackSettings(map[string]any{"catalog_fallback": value}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	setting, err := CatalogFallbackSettings(map[string]any{"catalog_fallback": map[string]any{"mode": "custom", "models": []string{"gemini-3-pro-image", "veo-3.1", "veo-3.1"}}})
	if err != nil {
		t.Fatal(err)
	}
	models, source := FallbackCatalog(setting, SubscriptionObservation{Status: "ok", FreeTrial: &paid})
	if source != "custom" || len(models) != 2 || models[0].MediaKind != "image" || models[1].MediaKind != "video" {
		t.Fatal(models, source)
	}
	for _, mode := range []string{"custom", "disabled"} {
		models, _ = FallbackCatalog(CatalogFallback{Mode: mode, Models: []string{}}, SubscriptionObservation{Status: "ok", FreeTrial: &trial})
		if len(models) != 0 {
			t.Fatalf("%s supplied models", mode)
		}
	}
}

func TestEnterpriseMetadataDiagnosticsSanitizeAndRetainStatus(t *testing.T) {
	for reason, code := range map[string]string{"SERVICE_DISABLED": "service_disabled", "BILLING_DISABLED": "billing_disabled", "CONSUMER_SUSPENDED": "consumer_suspended", "IAM_PERMISSION_DENIED": "permission_denied"} {
		body := fmt.Sprintf(`{"error":{"message":"secret-token","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":%q,"metadata":{"service":"discoveryengine.googleapis.com","consumer":"projects/123","token":"secret-token"}}]}}`, reason)
		err := ParseMetadataError(403, []byte(body))
		encoded, _ := json.Marshal(err)
		if err.Code != code || err.ConsumerProject != "123" || err.HTTPStatus != 403 || strings.Contains(string(encoded), "secret-token") {
			t.Fatalf("%s", encoded)
		}
	}
	wrong := ParseMetadataError(403, []byte(`{"error":{"message":"BILLING_DISABLED","details":[{"@type":"untrusted","domain":"googleapis.com","reason":"BILLING_DISABLED"}]}}`))
	if wrong.Code != "permission_denied" || wrong.Reason != "" {
		t.Fatal(wrong)
	}
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return resourceResponse(403, `{"error":{"message":"secret"}}`), nil
	})}
	status, err := resourceJSON(context.Background(), client, Origin+"/v1/projects/123/locations/global/collections", "synthetic", "123", &json.RawMessage{})
	if status != 403 || err != nil {
		t.Fatalf("legacy status contract changed: %d %v", status, err)
	}
	status, err = resourceJSONDiagnostic(context.Background(), client, Origin+"/v1/projects/123/locations/global/collections", "synthetic", "123", &json.RawMessage{})
	if status != 403 || err == nil {
		t.Fatalf("missing diagnostic: %d %v", status, err)
	}
}

func TestEnterpriseDiscoveryFiveProjectsAndSavedScopeOnCRMFailure(t *testing.T) {
	application := config.GeminiEnterpriseOAuth{ClientID: "fixture-client"}
	crmFailed := false
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatal("discovery performed a mutation")
		}
		if req.URL.Host == "cloudresourcemanager.googleapis.com" {
			if req.Header.Get("X-Goog-User-Project") != "" {
				t.Fatal("CRM consumer overridden")
			}
			if crmFailed {
				return resourceResponse(503, `{}`), nil
			}
			return resourceResponse(200, `{"projects":[{"name":"projects/123","state":"ACTIVE"},{"name":"projects/234","state":"ACTIVE"},{"name":"projects/345","state":"ACTIVE"},{"name":"projects/456","state":"ACTIVE"},{"name":"projects/567","state":"ACTIVE"}]}`), nil
		}
		project := strings.Split(req.URL.Path, "/")[3]
		if req.Header.Get("X-Goog-User-Project") != project {
			t.Fatal("Discovery Engine consumer missing")
		}
		if project != "123" && project != "234" {
			return resourceResponse(403, fmt.Sprintf(`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"googleapis.com","reason":"SERVICE_DISABLED","metadata":{"service":"discoveryengine.googleapis.com","consumer":"projects/%s"}}]}}`, project)), nil
		}
		parent := strings.TrimPrefix(req.URL.Path, "/v1alpha/")
		if strings.HasSuffix(parent, "/collections") {
			return discoveryListResponse("collections", strings.TrimSuffix(parent, "/collections")+"/collections/default_collection"), nil
		}
		parent = strings.TrimPrefix(req.URL.Path, "/v1/")
		if strings.HasSuffix(parent, "/engines") {
			return discoveryListResponse("engines", strings.TrimSuffix(parent, "/engines")+"/engines/engine"), nil
		}
		return discoveryListResponse("assistants", strings.TrimSuffix(parent, "/assistants")+"/assistants/default_assistant"), nil
	})}
	result, err := DiscoverResources(t.Context(), client, application, "synthetic", nil)
	if err != nil || result.Status != "partial" || len(result.Warnings) != 3*len(accountDiscoveryLocations) || len(result.Collections) != 2*len(accountDiscoveryLocations) {
		t.Fatalf("collections=%d warnings=%d err=%v", len(result.Collections), len(result.Warnings), err)
	}
	for _, warning := range result.Warnings {
		if warning.Code != "service_disabled" || warning.ProjectID != warning.ConsumerProject {
			t.Fatal(warning)
		}
	}
	old := result.Collections[0]
	old.Disabled = true
	old.Settings = map[string]any{"prefix": "preserved", "catalog_fallback": map[string]any{"mode": "disabled"}}
	crmFailed = true
	result, err = DiscoverResources(t.Context(), client, application, "synthetic", []Collection{old})
	if err != nil || result.Status != "partial" || len(result.Collections) == 0 {
		t.Fatalf("saved scope not probed: %+v %v", result, err)
	}
	account, _ := MergeDiscoveredCollections(Account{Version: 1, ID: "fixture", Revision: 1, Collections: []Collection{old}}, result)
	for _, collection := range account.Collections {
		if collection.ID == old.ID && (!collection.Disabled || collection.Settings["prefix"] != "preserved") {
			t.Fatal("saved preferences lost")
		}
	}
}
