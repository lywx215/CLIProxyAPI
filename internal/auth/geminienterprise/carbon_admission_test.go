package geminienterprise

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCarbonSettingsDefaultStrictBoolAndSafeSummary(t *testing.T) {
	settings, err := ReadCarbonSettings(map[string]any{})
	if err != nil || settings.Enabled || settings.Evidence != nil {
		t.Fatal(settings, err)
	}
	for _, value := range []any{"true", "false", 1, 0, nil, []bool{true}} {
		if _, err = ReadCarbonSettings(map[string]any{CarbonEnabledKey: value}); err == nil {
			t.Fatalf("accepted invalid Carbon bool %T", value)
		}
	}
	metadata := map[string]any{"client_id": "client", "email": "person@example.test", "refresh_token": "synthetic-refresh", "access_token": "short-lived-access"}
	first := CarbonAuthorizationFingerprint(metadata)
	metadata["access_token"] = "new-short-lived-access"
	if first == "" || CarbonAuthorizationFingerprint(metadata) != first {
		t.Fatal("short access token bound persisted Carbon evidence")
	}
	delete(metadata, "refresh_token")
	if CarbonAuthorizationFingerprint(metadata) != "" {
		t.Fatal("access-only credentials received a persistent authorization identity")
	}
	evidence := &CarbonTestEvidence{CarbonTestSummary: CarbonTestSummary{TestID: "server-test", Status: "verified", CheckedAt: time.Unix(100, 0).UTC(), UpstreamModel: CarbonModel, HTTPStatus: 200}, Version: 1, AuthorizationFingerprint: strings.Repeat("a", 64), BindingFingerprint: strings.Repeat("b", 64), ConfigFingerprint: strings.Repeat("c", 64)}
	if !ValidCarbonEvidence(evidence) {
		t.Fatal("valid evidence rejected")
	}
	encoded, _ := json.Marshal(evidence.Summary())
	if strings.Contains(string(encoded), "fingerprint") || strings.Contains(string(encoded), "synthetic-refresh") || !strings.Contains(string(encoded), "server-test") {
		t.Fatal(string(encoded))
	}
	evidence.UpstreamModel = ArgonModel
	if ValidCarbonEvidence(evidence) {
		t.Fatal("Argon success was accepted as Carbon evidence")
	}
}

func TestCarbonBindingFingerprintIncludesEveryApplicationResource(t *testing.T) {
	binding := config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "123", Location: "global", CollectionID: "collection", EngineID: "engine", AssistantID: "assistant"}
	baseline := CarbonBindingFingerprint(binding)
	for _, change := range []func(*config.GeminiEnterpriseOAuth){
		func(value *config.GeminiEnterpriseOAuth) { value.ProjectID = "456" },
		func(value *config.GeminiEnterpriseOAuth) { value.Location = "us" },
		func(value *config.GeminiEnterpriseOAuth) { value.CollectionID = "different" },
		func(value *config.GeminiEnterpriseOAuth) { value.EngineID = "different" },
		func(value *config.GeminiEnterpriseOAuth) { value.AssistantID = "different" },
	} {
		other := binding
		change(&other)
		if CarbonBindingFingerprint(other) == baseline {
			t.Fatal("incomplete application fingerprint")
		}
	}
}

func TestCarbonAccountReadAndLegacyAdaptationRejectNonBooleanSettings(t *testing.T) {
	metadata := map[string]any{"email": "person@example.test", "client_id": "client", "project_id": "123", "engine_id": "app"}
	account, err := AccountFromLegacy(metadata)
	if err != nil || len(account.Collections) != 1 {
		t.Fatal(account, err)
	}
	for _, value := range []any{"true", "false", 1, 0, nil, []bool{true}} {
		metadata[CarbonEnabledKey] = value
		if _, err = AccountFromLegacy(metadata); err == nil {
			t.Fatalf("legacy adapter accepted Carbon setting %T", value)
		}
		invalid := CloneAccount(account)
		invalid.Collections[0].Settings = map[string]any{CarbonEnabledKey: value}
		if _, err = ReadAccount(PutAccount(metadata, invalid)); err == nil {
			t.Fatalf("account reader accepted Carbon setting %T", value)
		}
	}
	for _, value := range []bool{false, true} {
		metadata[CarbonEnabledKey] = value
		if _, err = AccountFromLegacy(metadata); err != nil {
			t.Fatalf("boolean legacy setting rejected: %v", err)
		}
		account.Collections[0].Settings = map[string]any{CarbonEnabledKey: value}
		if _, err = ReadAccount(PutAccount(metadata, account)); err != nil {
			t.Fatalf("boolean collection setting rejected: %v", err)
		}
	}
}
