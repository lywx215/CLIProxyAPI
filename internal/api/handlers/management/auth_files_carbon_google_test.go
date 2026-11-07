package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type carbonGoogleManagementMock struct {
	mu     sync.Mutex
	calls  int
	patch  int
	values map[string]map[string]any
	before func(*http.Request)
}

func mockCarbonGoogleManagement(t *testing.T) *carbonGoogleManagementMock {
	t.Helper()
	mock := &carbonGoogleManagementMock{values: map[string]map[string]any{}}
	previous := newGeminiEnterpriseResourceClient
	t.Cleanup(func() { newGeminiEnterpriseResourceClient = previous })
	newGeminiEnterpriseResourceClient = func(string) (*http.Client, error) {
		return &http.Client{Transport: carbonSessionRoundTrip(func(request *http.Request) (*http.Response, error) {
			if mock.before != nil {
				mock.before(request)
			}
			mock.mu.Lock()
			defer mock.mu.Unlock()
			mock.calls++
			if !ge.OfficialResource(request) || request.Header.Get("Authorization") == "" || (request.Method != "GET" && request.Method != "PATCH") {
				t.Error("Google configuration escaped its scoped management operation")
			}
			name := strings.TrimPrefix(request.URL.Path, "/v1alpha/")
			value := mock.values[name]
			if value == nil {
				value = map[string]any{"name": name}
				if strings.Contains(name, "/assistants/") {
					value["generationConfig"] = map[string]any{"allowedModelIds": []any{ge.ArgonModel}, "defaultModelId": ge.ArgonModel}
					value["webGroundingType"] = "WEB_GROUNDING_TYPE_GOOGLE_SEARCH"
				} else {
					value["modelConfigs"] = map[string]any{ge.ArgonModel: "MODEL_ENABLED"}
				}
				mock.values[name] = value
			}
			if request.Method == "PATCH" {
				mock.patch++
				var payload map[string]any
				if json.NewDecoder(request.Body).Decode(&payload) != nil {
					t.Error("invalid PATCH body")
				}
				if strings.Contains(name, "/assistants/") {
					value["generationConfig"].(map[string]any)["allowedModelIds"] = payload["generationConfig"].(map[string]any)["allowedModelIds"]
				} else {
					value["modelConfigs"] = payload["modelConfigs"]
				}
			}
			data, _ := json.Marshal(value)
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
		})}, nil
	}
	return mock
}

func carbonGoogleManagementRequest(root *coreauth.Auth, children ...*coreauth.Auth) carbonGoogleConfigureRequest {
	account, _ := ge.AccountFromLegacy(root.Metadata)
	request := carbonGoogleConfigureRequest{Name: root.FileName, AuthIndex: lockedAuthIndex(root), AccountID: account.ID, ExpectedRevision: account.Revision}
	for _, child := range children {
		bindingID := coreauth.EnterpriseBindingID(child)
		for _, collection := range account.Collections {
			if collection.ID == bindingID {
				request.Applications = append(request.Applications, carbonGoogleApplicationRequest{BindingID: bindingID, AuthIndex: lockedAuthIndex(child), ExpectedRevision: collection.Revision, Expected: collection.ResourceBinding})
			}
		}
	}
	return request
}

func carbonGoogleCall(t *testing.T, ctx context.Context, h *Handler, request carbonGoogleConfigureRequest) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/", h.ConfigureGeminiEnterpriseCarbonGoogle)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("POST", "/", strings.NewReader(string(data))).WithContext(ctx))
	return recorder
}

func TestCarbonGoogleManagementConfiguresOnlyImportedAppsAndPreservesLocalAdmission(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	lease, err := h.authManager.BeginEnterpriseCarbonTest(children[0], 1)
	if err != nil {
		t.Fatal(err)
	}
	root, err = h.authManager.RecordEnterpriseCarbonTestEvidence(t.Context(), lease, &ge.CarbonTestEvidence{CarbonTestSummary: ge.CarbonTestSummary{Status: "verified", UpstreamModel: ge.CarbonModel, HTTPStatus: 200}})
	if err != nil {
		t.Fatal(err)
	}
	children = h.authManager.EnterpriseCollections(root.ID)
	mock := mockCarbonGoogleManagement(t)
	before, _ := h.authManager.GetByID(root.ID)
	request := carbonGoogleManagementRequest(root, children...)
	response := carbonGoogleCall(t, t.Context(), h, request)
	if response.Code != 200 || gjson.GetBytes(response.Body.Bytes(), "applications.#").Int() != 3 || gjson.GetBytes(response.Body.Bytes(), "status").String() != "completed" || !gjson.GetBytes(response.Body.Bytes(), "google_carbon_configure_supported").Bool() || mock.patch != 6 {
		t.Fatalf("bounded configuration failed: %d %s", response.Code, response.Body.String())
	}
	for _, item := range gjson.GetBytes(response.Body.Bytes(), "applications").Array() {
		if item.Get("status").String() != "configured" || item.Get("expected_revision").Uint() != 1 || item.Get("revision").Uint() != 1 {
			t.Fatal("configuration reported invalid app state")
		}
	}
	after, _ := h.authManager.GetByID(root.ID)
	if !reflect.DeepEqual(before.Metadata, after.Metadata) || before.Disabled != after.Disabled || before.Success != after.Success || before.Failed != after.Failed {
		t.Fatal("Google configuration changed local proof, switches, status or model usage")
	}
	current, _ := h.authManager.GetByID(children[0].ID)
	if !h.authManager.EnterpriseCarbonEvidenceState(current).Valid || h.authManager.EnterpriseCarbonAdmissionView(current).Enabled {
		t.Fatal("Google configuration discarded the previous proof or enabled local traffic")
	}
	for _, secret := range []string{"synthetic-private", "fingerprint", "Authorization", "modelConfigs", "allowedModelIds"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("configuration response exposed internal authentication or raw Google data")
		}
	}
	request.SnapshotID = gjson.GetBytes(response.Body.Bytes(), "snapshot_id").String()
	response = carbonGoogleCall(t, t.Context(), h, request)
	if response.Code != 200 || mock.patch != 6 || gjson.GetBytes(response.Body.Bytes(), "applications.0.status").String() != "unchanged" {
		t.Fatalf("same snapshot was not idempotent: %s", response.Body.String())
	}
	request.Applications = append(request.Applications, request.Applications...)
	response = carbonGoogleCall(t, t.Context(), h, request)
	if response.Code != 400 || mock.patch != 6 {
		t.Fatal("oversized batch was not rejected before Google")
	}
}

func TestCarbonGoogleManagementImplicitAssistantDoesNotWriteAllowlistOrAdmitTraffic(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	mock := mockCarbonGoogleManagement(t)
	binding := ge.FromMetadata(children[0].Metadata).WithDefaults()
	assistant := ge.Engine(binding) + "/assistants/" + binding.AssistantID
	mock.values[assistant] = map[string]any{
		"name":             assistant,
		"generationConfig": map[string]any{"defaultModelId": ge.ArgonModel, "allowedModelIds": []any{}},
		"webGroundingType": "WEB_GROUNDING_TYPE_GOOGLE_SEARCH",
	}
	original, _ := json.Marshal(mock.values[assistant])
	before, _ := h.authManager.GetByID(root.ID)
	response := carbonGoogleCall(t, t.Context(), h, carbonGoogleManagementRequest(root, children[0]))
	if response.Code != 200 || gjson.GetBytes(response.Body.Bytes(), "status").String() != "partial" || gjson.GetBytes(response.Body.Bytes(), "applications.0.status").String() != "verification_required" || gjson.GetBytes(response.Body.Bytes(), "applications.0.reason").String() != "assistant_defaults_preserved" || mock.patch != 1 {
		t.Fatalf("implicit Assistant was not reported as pending verification: %d %s", response.Code, response.Body.String())
	}
	current, _ := json.Marshal(mock.values[assistant])
	after, _ := h.authManager.GetByID(root.ID)
	if string(current) != string(original) || !reflect.DeepEqual(before.Metadata, after.Metadata) {
		t.Fatal("Engine-only configuration changed the Assistant or local credential settings")
	}
	child, _ := h.authManager.GetByID(children[0].ID)
	view := h.authManager.EnterpriseCarbonAdmissionView(child)
	if view.Enabled || view.Available || h.authManager.EnterpriseCarbonEvidenceState(child).Valid {
		t.Fatal("pending configuration became model-call evidence or enabled Carbon")
	}
}

func TestCarbonGoogleManagementFrozenBindingsAndExpiryRejectBeforeGoogle(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	mock := mockCarbonGoogleManagement(t)
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	h.enterpriseCarbonGoogle.now = func() time.Time { return now }
	request := carbonGoogleManagementRequest(root, children[0])
	response := carbonGoogleCall(t, t.Context(), h, request)
	snapshotID := gjson.GetBytes(response.Body.Bytes(), "snapshot_id").String()
	if response.Code != 200 || snapshotID == "" {
		t.Fatal(response.Body.String())
	}
	initialCalls := mock.calls
	foreign := request
	foreign.Applications = append([]carbonGoogleApplicationRequest(nil), request.Applications...)
	foreign.Applications[0].Expected.EngineID = "not-imported"
	foreign.SnapshotID = snapshotID
	if result := carbonGoogleCall(t, t.Context(), h, foreign); result.Code != 409 || mock.calls != initialCalls {
		t.Fatal("foreign resource reached Google")
	}
	request.SnapshotID = snapshotID
	now = now.Add(carbonGoogleSnapshotLifetime)
	response = carbonGoogleCall(t, t.Context(), h, request)
	if response.Code != 409 || gjson.GetBytes(response.Body.Bytes(), "code").String() != "snapshot_expired" || mock.calls != initialCalls {
		t.Fatal("expired snapshot reached Google")
	}
}

func TestCarbonGoogleManagementLegacyGraphDefaultsUseCompleteCanonicalResources(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	account, err := ge.ReadAccount(root.Metadata)
	if err != nil || account == nil {
		t.Fatal(err)
	}
	account.Collections[0].Location = ""
	account.Collections[0].CollectionID = ""
	root.Metadata = ge.PutAccount(root.Metadata, *account)
	root, err = h.authManager.Update(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	child, exists := h.authManager.GetByID(children[0].ID)
	if !exists {
		t.Fatal("legacy graph lost its application projection")
	}
	request := carbonGoogleManagementRequest(root, child)
	request.Applications[0].Expected = ge.Resources(ge.FromMetadata(child.Metadata))
	if request.Applications[0].Expected.Location != "global" || request.Applications[0].Expected.CollectionID != "default_collection" {
		t.Fatal("legacy child did not resolve the documented resource defaults")
	}
	mock := mockCarbonGoogleManagement(t)
	response := carbonGoogleCall(t, t.Context(), h, request)
	if response.Code != 200 || gjson.GetBytes(response.Body.Bytes(), "applications.0.status").String() != "configured" || mock.patch != 2 {
		t.Fatalf("canonical legacy graph target was rejected: %d %s", response.Code, response.Body.String())
	}
	request.SnapshotID = gjson.GetBytes(response.Body.Bytes(), "snapshot_id").String()
	initialCalls := mock.calls
	request.Applications[0].Expected.Location = ""
	response = carbonGoogleCall(t, t.Context(), h, request)
	if response.Code != 409 || mock.calls != initialCalls {
		t.Fatal("legacy compatibility accepted an incomplete client binding")
	}
	request.Applications[0].Expected.Location = "global"
	request.Applications[0].Expected.CollectionID = "foreign-collection"
	response = carbonGoogleCall(t, t.Context(), h, request)
	if response.Code != 409 || mock.calls != initialCalls {
		t.Fatal("legacy compatibility accepted a different collection")
	}
}

func TestCarbonGoogleManagementSnapshotAllowsTrustedRefreshButRejectsReplacement(t *testing.T) {
	for _, mode := range []string{"access-refresh", "refresh-rotation", "manual-replacement", "explicit-reauthorization"} {
		t.Run(mode, func(t *testing.T) {
			h, root, children := enterpriseAccountFixture(t)
			mock := mockCarbonGoogleManagement(t)
			request := carbonGoogleManagementRequest(root, children[0])
			response := carbonGoogleCall(t, t.Context(), h, request)
			request.SnapshotID = gjson.GetBytes(response.Body.Bytes(), "snapshot_id").String()
			if response.Code != 200 || request.SnapshotID == "" {
				t.Fatal(response.Body.String())
			}
			current, _ := h.authManager.GetByID(root.ID)
			updated := current.Clone()
			updated.Metadata["access_token"] = "fresh-synthetic-private-access"
			var err error
			switch mode {
			case "access-refresh":
				_, err = h.authManager.UpdateRefreshedAuth(t.Context(), current, updated)
			case "refresh-rotation":
				updated.Metadata["refresh_token"] = "rotated-synthetic-refresh"
				_, err = h.authManager.UpdateRefreshedAuth(t.Context(), current, updated)
			case "manual-replacement":
				updated.Metadata["refresh_token"] = "replacement-synthetic-refresh"
				_, err = h.authManager.Update(t.Context(), updated)
			case "explicit-reauthorization":
				_, err = h.authManager.ReauthorizeEnterpriseAccount(t.Context(), updated)
			}
			if err != nil {
				t.Fatal(err)
			}
			initialCalls := mock.calls
			request.Applications = carbonGoogleManagementRequest(root, children[2]).Applications
			response = carbonGoogleCall(t, t.Context(), h, request)
			status := gjson.GetBytes(response.Body.Bytes(), "applications.0.status").String()
			if mode == "access-refresh" || mode == "refresh-rotation" {
				if response.Code != 200 || status != "configured" || mock.calls <= initialCalls {
					t.Fatalf("trusted refresh invalidated a fixed configuration snapshot: %s", response.Body.String())
				}
			} else if response.Code != 200 || status != "changed" || mock.calls != initialCalls {
				t.Fatalf("authorization replacement reused old snapshot: %s", response.Body.String())
			}
		})
	}
}

func TestCarbonGoogleManagementChangeOrCancellationStopsUnsubmittedWrites(t *testing.T) {
	for _, scenario := range []string{"binding-change", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			h, root, children := enterpriseAccountFixture(t)
			mock := mockCarbonGoogleManagement(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var once sync.Once
			mock.before = func(*http.Request) {
				once.Do(func() {
					if scenario == "cancel" {
						cancel()
					} else if _, err := h.authManager.UpdateEnterpriseAccount(t.Context(), root, 0, func(account *ge.Account) error { account.Collections[0].EngineID = "replacement-app"; return nil }); err != nil {
						t.Error(err)
					}
				})
			}
			response := carbonGoogleCall(t, ctx, h, carbonGoogleManagementRequest(root, children[0]))
			if mock.patch != 0 || h.enterpriseCarbonGoogle.active != 0 || len(h.enterpriseCarbonGoogle.running) != 0 || len(h.enterpriseCarbonGoogle.engines) != 0 {
				t.Fatal("cancellation/change wrote or leaked foreground admission")
			}
			if scenario == "binding-change" && gjson.GetBytes(response.Body.Bytes(), "applications.0.status").String() != "changed" {
				t.Fatal(response.Body.String())
			}
		})
	}
}

func TestCarbonGoogleManagementDuplicateAndGlobalConcurrencyAreBounded(t *testing.T) {
	var coordinator enterpriseCarbonGoogleCoordinator
	releaseA, reason := coordinator.begin("account-a")
	if reason != "" {
		t.Fatal(reason)
	}
	if release, reason := coordinator.begin("account-a"); release != nil || reason != "operation_in_progress" {
		t.Fatal("duplicate credential operation was admitted")
	}
	releaseB, reason := coordinator.begin("account-b")
	if reason != "" {
		t.Fatal(reason)
	}
	if release, reason := coordinator.begin("account-c"); release != nil || reason != "configure_busy" {
		t.Fatal("third simultaneous configuration operation was admitted")
	}
	locked, err := coordinator.lockEngine(t.Context(), "same-engine")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if release, err := coordinator.lockEngine(cancelled, "same-engine"); release != nil || err == nil {
		t.Fatal("canceled same-resource operation entered")
	}
	locked()
	releaseA()
	releaseA()
	releaseB()
	if coordinator.active != 0 || len(coordinator.running) != 0 || len(coordinator.engines) != 0 {
		t.Fatal("configuration admission or resource locks leaked")
	}
}
