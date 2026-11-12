package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type carbonSessionRoundTrip func(*http.Request) (*http.Response, error)

func (f carbonSessionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCarbonManagementFixedTestProofAndManualSwitch(t *testing.T) {
	for _, scenario := range []string{"verified", "verified-trial", "wrong-nonce", "no-session", "wrong-model", "missing-model", "foreign-session", "truncated", "model-changed", "read-forbidden", "read-redirect", "oversized-proof"} {
		t.Run(scenario, func(t *testing.T) {
			h, root, children := enterpriseAccountFixture(t)
			child := children[0]
			trial := scenario == "verified-trial"
			publishManagementEnterpriseSubscription(t, h.authManager, child, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial}, ge.Model{ID: ge.ArgonModel})
			if _, err := h.authManager.UpdateEnterpriseCarbonEnabled(t.Context(), child, 1, false, ""); err != nil {
				t.Fatal(err)
			}
			child, _ = h.authManager.GetByID(child.ID)
			const initialRevision = 2
			calls, reads := 0, 0
			readStatus := http.StatusOK
			nonce := ""
			session := ge.Engine(ge.FromMetadata(child.Metadata)) + "/sessions/carbon-test"
			h.authManager.RegisterExecutor(&enterpriseProbeExecutor{execute: func(ctx context.Context, selected *coreauth.Auth, request coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
				calls++
				if selected.ID != child.ID || request.Model != ge.CarbonModel || !coreauth.EnterpriseCarbonModelTestActive(ctx) || opts.Metadata[coreexec.PinnedAuthMetadataKey] != child.ID {
					t.Fatal("Carbon probe lost its internal fixed-application scope")
				}
				coreexec.MarkUpstreamAttempt(ctx)
				nonce = regexp.MustCompile(`CARBON_[a-f0-9]{32}`).FindString(string(request.Payload))
				if nonce == "" {
					t.Fatal("missing independent verification code")
				}
				if scenario == "model-changed" {
					return coreexec.Response{}, &coreauth.EnterpriseModelProbeError{Code: "model_changed", Message: "changed by fixture", HTTPStatus: 409}
				}
				text, stop := nonce, "STOP"
				if scenario == "wrong-nonce" {
					text = "incorrect"
				}
				if scenario == "truncated" {
					stop = ""
				}
				payload, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"finishReason": stop, "content": map[string]any{"parts": []any{map[string]string{"text": text}}}}}})
				metadata := map[string]any{"enterprise_carbon_model": ge.CarbonModel, "enterprise_carbon_session": session}
				if scenario == "no-session" {
					delete(metadata, "enterprise_carbon_session")
				}
				if scenario == "foreign-session" {
					metadata["enterprise_carbon_session"] = "projects/other/locations/global/collections/default_collection/engines/foreign/sessions/test"
				}
				return coreexec.Response{Payload: payload, Metadata: metadata}, nil
			}})
			previous := newGeminiEnterpriseResourceClient
			t.Cleanup(func() { newGeminiEnterpriseResourceClient = previous })
			newGeminiEnterpriseResourceClient = func(string) (*http.Client, error) {
				return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: carbonSessionRoundTrip(func(request *http.Request) (*http.Response, error) {
					reads++
					if request.Method != "GET" || request.URL.Host != "discoveryengine.googleapis.com" || request.Header.Get("Authorization") != "Bearer synthetic-private-access" || !strings.Contains(request.URL.Query().Get("fields"), "queryConfig") {
						t.Fatal("session proof escaped official bound read")
					}
					if scenario == "read-forbidden" {
						readStatus = http.StatusForbidden
					}
					if scenario == "read-redirect" {
						readStatus = http.StatusFound
					}
					if readStatus != http.StatusOK {
						return &http.Response{StatusCode: readStatus, Header: http.Header{"Location": {"https://untrusted.example.test"}}, Body: io.NopCloser(strings.NewReader("not a model proof"))}, nil
					}
					if scenario == "oversized-proof" {
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 2*1024*1024+1)))}, nil
					}
					actual := ge.CarbonModel
					if scenario == "wrong-model" {
						actual = ge.ArgonModel
					}
					queryConfig := map[string]any{"google.discoveryengine.googleapis.com.Assistant.model_id": actual}
					if scenario == "missing-model" {
						queryConfig = map[string]any{}
					}
					payload, _ := json.Marshal(map[string]any{"name": session, "turns": []any{map[string]any{"query": map[string]string{"text": "verification " + nonce}, "queryConfig": queryConfig}}})
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
				})}, nil
			}
			request := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": initialRevision, "expected": ge.Resources(ge.FromMetadata(child.Metadata)), "model": ge.CarbonModel}
			response := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
			expectedStatus := http.StatusOK
			if scenario == "model-changed" {
				expectedStatus = http.StatusConflict
			}
			if response.Code != expectedStatus || calls != 1 {
				t.Fatalf("fixed closed candidate failed: %d %s calls=%d", response.Code, response.Body.String(), calls)
			}
			currentChild, _ := h.authManager.GetByID(child.ID)
			admission := h.authManager.EnterpriseCarbonAdmissionView(currentChild)
			if admission.Enabled || admission.Available {
				t.Fatal("test automatically enabled Carbon")
			}
			if scenario != "verified" && scenario != "verified-trial" {
				if !admission.CanEnable || admission.VerificationRequired || gjson.GetBytes(response.Body.Bytes(), "status").String() == "ok" {
					t.Fatalf("failed diagnostics blocked manual admission or falsely reported success: %s", response.Body.String())
				}
				return
			}
			if !admission.CanEnable || admission.Test == nil || admission.Test.Status != "verified" || admission.Test.UpstreamModel != ge.CarbonModel || reads != 1 {
				t.Fatalf("saved verification incomplete: %+v reads=%d body=%s", admission, reads, response.Body.String())
			}
			if gjson.GetBytes(response.Body.Bytes(), "expected_revision").Uint() != initialRevision || gjson.GetBytes(response.Body.Bytes(), "revision").Uint() != initialRevision {
				t.Fatal("observation changed collection edit revision")
			}
			toggle := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": initialRevision, "fields": map[string]any{"carbon_enabled": true}}
			changed := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", toggle)
			if changed.Code != 200 {
				t.Fatalf("manual enable failed: %s", changed.Body.String())
			}
			current, _ := h.enterpriseTarget(root.FileName, child.Index, coreauth.EnterpriseBindingID(child))
			if !h.authManager.EnterpriseCarbonAdmissionView(current).Available {
				t.Fatal("successful manual switch was not effective")
			}
			toggle["expected_revision"], toggle["fields"] = initialRevision+1, map[string]any{"carbon_enabled": false}
			delete(toggle, "carbon_test_id")
			closed := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", toggle)
			if closed.Code != 200 {
				t.Fatalf("manual disable failed: %s", closed.Body.String())
			}
			current, _ = h.enterpriseTarget(root.FileName, child.Index, coreauth.EnterpriseBindingID(child))
			state := h.authManager.EnterpriseCarbonAdmissionView(current)
			if state.Enabled || state.Available || !state.CanEnable || state.Test.TestID != admission.Test.TestID || calls != 1 || reads != 1 {
				t.Fatal("closing reset proof or generated extra content")
			}
			// A temporary session-read failure must not revoke an existing proof.
			readStatus = http.StatusServiceUnavailable
			request["expected_revision"] = initialRevision + 2
			retry := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
			current, _ = h.enterpriseTarget(root.FileName, child.Index, coreauth.EnterpriseBindingID(child))
			state = h.authManager.EnterpriseCarbonAdmissionView(current)
			if retry.Code != http.StatusOK || gjson.GetBytes(retry.Body.Bytes(), "status").String() == "ok" || !state.CanEnable || state.Test.TestID != admission.Test.TestID {
				t.Fatalf("temporary proof-read failure revoked verified evidence: %s", retry.Body.String())
			}
			// An actual model mismatch, including a 502 result, is not temporary.
			readStatus, scenario = http.StatusOK, "wrong-model"
			mismatch := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
			current, _ = h.enterpriseTarget(root.FileName, child.Index, coreauth.EnterpriseBindingID(child))
			state = h.authManager.EnterpriseCarbonAdmissionView(current)
			if mismatch.Code != http.StatusOK || !state.CanEnable || state.Test == nil || state.Test.Status != "incomplete" {
				t.Fatalf("wrong model was not recorded diagnostically or blocked manual enablement: %s", mismatch.Body.String())
			}
		})
	}
}

func TestCarbonDirectModelTestUsesRegisteredModelWithoutVerification(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	child := children[0]
	paid := false
	publishManagementEnterpriseSubscription(t, h.authManager, child, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &paid}, ge.Model{ID: ge.ArgonModel}, ge.Model{ID: ge.CarbonModel})
	const alias = "carbon-public"
	h.authManager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{ge.Provider: {{Name: ge.CarbonModel, Alias: alias, Fork: true}}})
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(child.ID, ge.Provider, []*registry.ModelInfo{
		{ID: ge.CarbonModel}, {ID: alias, MetadataModelID: ge.CarbonModel},
		{ID: ge.ArgonModel},
	})
	t.Cleanup(func() { reg.UnregisterClient(child.ID) })
	calls := 0
	h.authManager.RegisterExecutor(&enterpriseProbeExecutor{execute: func(ctx context.Context, selected *coreauth.Auth, req coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
		calls++
		if selected.ID != child.ID || opts.Metadata[coreexec.PinnedAuthMetadataKey] != child.ID ||
			coreauth.EnterpriseCarbonModelTestActive(ctx) || strings.Contains(string(req.Payload), "CARBON_") {
			t.Fatal("direct Carbon test lost its fixed application or used dedicated verification")
		}
		if err := coreauth.ValidateEnterpriseModelRequest(ctx, selected, ge.CarbonModel); err != nil {
			t.Fatalf("paid Carbon was denied at generation: %v", err)
		}
		coreexec.MarkUpstreamAttempt(ctx)
		return coreexec.Response{Payload: []byte(`{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"OK"}]}}]}`)}, nil
	}})
	bindingID := coreauth.EnterpriseBindingID(child)
	request := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": bindingID, "expected_revision": 1, "expected": ge.Resources(ge.FromMetadata(child.Metadata)), "carbon_direct": true}
	probe := func(model string) *httptest.ResponseRecorder {
		t.Helper()
		request["model"] = model
		return enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, http.MethodPost, request)
	}
	for _, model := range []string{ge.CarbonModel, alias} {
		response := probe(model)
		result := gjson.ParseBytes(response.Body.Bytes())
		if response.Code != http.StatusOK || result.Get("status").String() != "ok" || result.Get("model").String() != model || result.Get("upstream_model").String() != ge.CarbonModel || result.Get("carbon_test_id").Exists() {
			t.Fatalf("ordinary Carbon generation failed: %d %s", response.Code, result.Raw)
		}
	}
	if response := probe(ge.ArgonModel); response.Code != http.StatusConflict || gjson.GetBytes(response.Body.Bytes(), "code").String() != "model_changed" {
		t.Fatalf("Carbon test accepted a different mapped model: %s", response.Body.String())
	}
	toggle := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": bindingID, "expected_revision": 1, "fields": map[string]any{ge.CarbonEnabledKey: false}}
	if response := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, http.MethodPatch, toggle); response.Code != http.StatusOK {
		t.Fatalf("closing a default-enabled Carbon failed: %s", response.Body.String())
	}
	request["expected_revision"] = 2
	if response := probe(ge.CarbonModel); response.Code != http.StatusOK || gjson.GetBytes(response.Body.Bytes(), "status").String() != "ok" || calls != 3 {
		t.Fatalf("legacy switch overrode normal Carbon catalog routing: %d %s (calls=%d)", response.Code, response.Body.String(), calls)
	}
}

func TestCarbonCandidateListIsSeparateAndReadOnly(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	child := children[0]
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ConfigV8ContextKey, true)
	c.Request = httptest.NewRequest("GET", "/?name="+root.FileName+"&auth_index="+child.Index+"&binding_id="+coreauth.EnterpriseBindingID(child), nil)
	h.GetAuthFileModels(c)
	if w.Code != 200 || !gjson.GetBytes(w.Body.Bytes(), "carbon_admission_supported").Bool() || gjson.GetBytes(w.Body.Bytes(), "test_candidates.0.id").String() != ge.CarbonModel || gjson.GetBytes(w.Body.Bytes(), "models.#").Int() != 0 || gjson.GetBytes(w.Body.Bytes(), "test_candidates.0.business_registered").Bool() {
		t.Fatalf("closed candidate leaked into business list: %s", w.Body.String())
	}
	for _, fields := range []map[string]json.RawMessage{{"carbon_enabled": json.RawMessage(`true`)}, {"enterprise_carbon_evidence.key": json.RawMessage(`{}`)}, {"enterprise_account.collections.0.settings.carbon_enabled": json.RawMessage(`true`)}} {
		if !containsCarbonManagedFields(fields) {
			t.Fatalf("generic edit could bypass dedicated app CAS: %v", fields)
		}
	}
}

func TestCarbonCredentialImportCannotForgeAdmission(t *testing.T) {
	data := []byte(`{"type":"gemini-enterprise","carbon_enabled":true,"enterprise_carbon_evidence":{"fake":{"status":"verified"}},"enterprise_account":{"collections":[{"settings":{"carbon_enabled":true,"carbon_test_evidence":{"status":"verified"}}}]},"note":"keep"}`)
	safe, err := sanitizeCarbonCredentialImport(data)
	if err != nil || strings.Contains(string(safe), "evidence") || gjson.GetBytes(safe, "carbon_enabled").Bool() || gjson.GetBytes(safe, "enterprise_account.collections.0.settings.carbon_enabled").Bool() || gjson.GetBytes(safe, "note").String() != "keep" {
		t.Fatalf("forged import survived: %s %v", safe, err)
	}
	if _, err = sanitizeCarbonCredentialImport([]byte(`{"type":"gemini-enterprise","carbon_enabled":"true"}`)); err == nil {
		t.Fatal("accepted non-boolean Carbon switch")
	}
	other := []byte(`{"type":"gemini-enterprise-web","carbon_enabled":true}`)
	if result, _ := sanitizeCarbonCredentialImport(other); string(result) != string(other) {
		t.Fatal("OAuth Carbon import changed Web credentials")
	}
	// The watcher normalizes provider case and whitespace before registration.
	variant := []byte(`{"type":" Gemini-Enterprise ","carbon_enabled":true,"enterprise_carbon_evidence":{"fake":{}}}`)
	if result, errVariant := sanitizeCarbonCredentialImport(variant); errVariant != nil || gjson.GetBytes(result, "carbon_enabled").Bool() || strings.Contains(string(result), "evidence") {
		t.Fatal("provider spelling bypassed the import boundary")
	}
	dir := t.TempDir()
	h := &Handler{authManager: coreauth.NewManager(nil, nil, nil), cfg: &config.Config{AuthDir: dir}}
	actual := []byte(`{"type":"gemini-enterprise","client_id":"fixture","project_id":"123","engine_id":"app","email":"fixture@example.test","access_token":"synthetic","refresh_token":"synthetic-refresh","carbon_enabled":true,"enterprise_carbon_evidence":{"forged":{"status":"verified"}}}`)
	if err = h.writeAuthFile(t.Context(), "import.json", actual); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "import.json"))
	if err != nil || strings.Contains(string(saved), "enterprise_carbon_evidence") || gjson.GetBytes(saved, "carbon_enabled").Bool() {
		t.Fatalf("watcher would restore forged import: %s %v", saved, err)
	}
}

func TestCarbonGenericMetadataEditsCannotForgeServerEvidence(t *testing.T) {
	for _, tagged := range []bool{true, false} {
		for _, field := range []string{"carbon_enabled", "enterprise_carbon_evidence.fake", "enterprise_account.collections.0.settings.carbon_enabled"} {
			h, root, children := enterpriseAccountFixture(t)
			before := h.authManager.EnterpriseCarbonAdmissionView(children[0])
			payload, _ := json.Marshal(map[string]any{"name": root.FileName, field: true})
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			if tagged {
				c.Set(ConfigV8ContextKey, true)
			}
			c.Request = httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(string(payload)))
			h.PatchAuthFileFields(c)
			after, _ := h.authManager.GetByID(children[0].ID)
			state := h.authManager.EnterpriseCarbonAdmissionView(after)
			if w.Code != http.StatusBadRequest || gjson.GetBytes(w.Body.Bytes(), "code").String() != "read_only_collection_fields" || state.Enabled != before.Enabled || state.Test != nil {
				t.Fatalf("generic metadata write bypassed the server-owned boundary (tagged=%v field=%s): %s", tagged, field, w.Body.String())
			}
		}
	}
}

func TestHydrogenManagementFixedTestProofAndManualSwitch(t *testing.T) {
	for _, scenario := range []string{"verified", "verified-trial", "wrong-nonce", "no-session", "wrong-model", "missing-model", "foreign-session", "truncated", "model-changed", "read-forbidden", "read-redirect", "oversized-proof"} {
		t.Run(scenario, func(t *testing.T) {
			h, root, children := enterpriseAccountFixture(t)
			child := children[0]
			trial := scenario == "verified-trial"
			publishManagementEnterpriseSubscription(t, h.authManager, child, ge.SubscriptionObservation{Status: "ok", Edition: "enterprise_plus", FreeTrial: &trial}, ge.Model{ID: ge.ArgonModel})
			if _, err := h.authManager.UpdateEnterpriseHydrogenEnabled(t.Context(), child, 1, false, ""); err != nil {
				t.Fatal(err)
			}
			child, _ = h.authManager.GetByID(child.ID)
			const initialRevision = 2
			calls, reads := 0, 0
			readStatus := http.StatusOK
			nonce := ""
			session := ge.Engine(ge.FromMetadata(child.Metadata)) + "/sessions/carbon-test"
			h.authManager.RegisterExecutor(&enterpriseProbeExecutor{execute: func(ctx context.Context, selected *coreauth.Auth, request coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
				calls++
				if selected.ID != child.ID || request.Model != ge.HydrogenModel || !coreauth.EnterpriseCarbonModelTestActive(ctx) || opts.Metadata[coreexec.PinnedAuthMetadataKey] != child.ID {
					t.Fatal("Carbon probe lost its internal fixed-application scope")
				}
				coreexec.MarkUpstreamAttempt(ctx)
				nonce = regexp.MustCompile(`HYDROGEN_[a-f0-9]{32}`).FindString(string(request.Payload))
				if nonce == "" {
					t.Fatal("missing independent verification code")
				}
				if scenario == "model-changed" {
					return coreexec.Response{}, &coreauth.EnterpriseModelProbeError{Code: "model_changed", Message: "changed by fixture", HTTPStatus: 409}
				}
				text, stop := nonce, "STOP"
				if scenario == "wrong-nonce" {
					text = "incorrect"
				}
				if scenario == "truncated" {
					stop = ""
				}
				payload, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"finishReason": stop, "content": map[string]any{"parts": []any{map[string]string{"text": text}}}}}})
				metadata := map[string]any{"enterprise_carbon_model": ge.HydrogenModel, "enterprise_carbon_session": session}
				if scenario == "no-session" {
					delete(metadata, "enterprise_carbon_session")
				}
				if scenario == "foreign-session" {
					metadata["enterprise_carbon_session"] = "projects/other/locations/global/collections/default_collection/engines/foreign/sessions/test"
				}
				return coreexec.Response{Payload: payload, Metadata: metadata}, nil
			}})
			previous := newGeminiEnterpriseResourceClient
			t.Cleanup(func() { newGeminiEnterpriseResourceClient = previous })
			newGeminiEnterpriseResourceClient = func(string) (*http.Client, error) {
				return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: carbonSessionRoundTrip(func(request *http.Request) (*http.Response, error) {
					reads++
					if request.Method != "GET" || request.URL.Host != "discoveryengine.googleapis.com" || request.Header.Get("Authorization") != "Bearer synthetic-private-access" || !strings.Contains(request.URL.Query().Get("fields"), "queryConfig") {
						t.Fatal("session proof escaped official bound read")
					}
					if scenario == "read-forbidden" {
						readStatus = http.StatusForbidden
					}
					if scenario == "read-redirect" {
						readStatus = http.StatusFound
					}
					if readStatus != http.StatusOK {
						return &http.Response{StatusCode: readStatus, Header: http.Header{"Location": {"https://untrusted.example.test"}}, Body: io.NopCloser(strings.NewReader("not a model proof"))}, nil
					}
					if scenario == "oversized-proof" {
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 2*1024*1024+1)))}, nil
					}
					actual := ge.HydrogenModel
					if scenario == "wrong-model" {
						actual = ge.ArgonModel
					}
					queryConfig := map[string]any{"google.discoveryengine.googleapis.com.Assistant.model_id": actual}
					if scenario == "missing-model" {
						queryConfig = map[string]any{}
					}
					payload, _ := json.Marshal(map[string]any{"name": session, "turns": []any{map[string]any{"query": map[string]string{"text": "verification " + nonce}, "queryConfig": queryConfig}}})
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
				})}, nil
			}
			request := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": initialRevision, "expected": ge.Resources(ge.FromMetadata(child.Metadata)), "model": ge.HydrogenModel}
			response := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
			expectedStatus := http.StatusOK
			if scenario == "model-changed" {
				expectedStatus = http.StatusConflict
			}
			if response.Code != expectedStatus || calls != 1 {
				t.Fatalf("fixed closed candidate failed: %d %s calls=%d", response.Code, response.Body.String(), calls)
			}
			currentChild, _ := h.authManager.GetByID(child.ID)
			admission := h.authManager.EnterpriseHydrogenAdmissionView(currentChild)
			if admission.Enabled || admission.Available {
				t.Fatal("test automatically enabled Carbon")
			}
			if scenario != "verified" && scenario != "verified-trial" {
				if !admission.CanEnable || admission.VerificationRequired || gjson.GetBytes(response.Body.Bytes(), "status").String() == "ok" {
					t.Fatalf("failed diagnostics blocked manual admission or falsely reported success: %s", response.Body.String())
				}
				return
			}
			if !admission.CanEnable || admission.Test == nil || admission.Test.Status != "verified" || admission.Test.UpstreamModel != ge.HydrogenModel || reads != 1 {
				t.Fatalf("saved verification incomplete: %+v reads=%d body=%s", admission, reads, response.Body.String())
			}
			if gjson.GetBytes(response.Body.Bytes(), "expected_revision").Uint() != initialRevision || gjson.GetBytes(response.Body.Bytes(), "revision").Uint() != initialRevision {
				t.Fatal("observation changed collection edit revision")
			}
			toggle := map[string]any{"name": root.FileName, "auth_index": child.Index, "binding_id": coreauth.EnterpriseBindingID(child), "expected_revision": initialRevision, "fields": map[string]any{"hydrogen_enabled": true}}
			changed := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", toggle)
			if changed.Code != 200 {
				t.Fatalf("manual enable failed: %s", changed.Body.String())
			}
			current, _ := h.enterpriseTarget(root.FileName, child.Index, coreauth.EnterpriseBindingID(child))
			if !h.authManager.EnterpriseHydrogenAdmissionView(current).Available {
				t.Fatal("successful manual switch was not effective")
			}
			toggle["expected_revision"], toggle["fields"] = initialRevision+1, map[string]any{"hydrogen_enabled": false}
			delete(toggle, "hydrogen_test_id")
			closed := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseCollection, "PATCH", toggle)
			if closed.Code != 200 {
				t.Fatalf("manual disable failed: %s", closed.Body.String())
			}
			current, _ = h.enterpriseTarget(root.FileName, child.Index, coreauth.EnterpriseBindingID(child))
			state := h.authManager.EnterpriseHydrogenAdmissionView(current)
			if state.Enabled || state.Available || !state.CanEnable || state.Test.TestID != admission.Test.TestID || calls != 1 || reads != 1 {
				t.Fatal("closing reset proof or generated extra content")
			}
			// A temporary session-read failure must not revoke an existing proof.
			readStatus = http.StatusServiceUnavailable
			request["expected_revision"] = initialRevision + 2
			retry := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
			current, _ = h.enterpriseTarget(root.FileName, child.Index, coreauth.EnterpriseBindingID(child))
			state = h.authManager.EnterpriseHydrogenAdmissionView(current)
			if retry.Code != http.StatusOK || gjson.GetBytes(retry.Body.Bytes(), "status").String() == "ok" || !state.CanEnable || state.Test.TestID != admission.Test.TestID {
				t.Fatalf("temporary proof-read failure revoked verified evidence: %s", retry.Body.String())
			}
			// An actual model mismatch, including a 502 result, is not temporary.
			readStatus, scenario = http.StatusOK, "wrong-model"
			mismatch := enterpriseManagementRequest(t, h.TestGeminiEnterpriseModel, "POST", request)
			current, _ = h.enterpriseTarget(root.FileName, child.Index, coreauth.EnterpriseBindingID(child))
			state = h.authManager.EnterpriseHydrogenAdmissionView(current)
			if mismatch.Code != http.StatusOK || !state.CanEnable || state.Test == nil || state.Test.Status != "incomplete" {
				t.Fatalf("wrong model was not recorded diagnostically or blocked manual enablement: %s", mismatch.Body.String())
			}
		})
	}
}
