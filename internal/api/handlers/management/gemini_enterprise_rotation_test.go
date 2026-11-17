package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type enterpriseRotationTransport func(*http.Request) (*http.Response, error)

func (t enterpriseRotationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t(request)
}

type enterpriseRotationRefreshExecutor struct {
	enterpriseProbeExecutor
	refresh func(*coreauth.Auth) (*coreauth.Auth, error)
}

func (e *enterpriseRotationRefreshExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return e.refresh(auth)
}

func disableEnterpriseRotationFixture(t *testing.T, h *Handler, root *coreauth.Auth) *coreauth.Auth {
	t.Helper()
	root.Disabled, root.Status, root.Metadata["disabled"] = true, coreauth.StatusDisabled, true
	disabled, err := h.authManager.Update(t.Context(), root)
	if err != nil || !disabled.Disabled {
		t.Fatalf("could not disable fixture account: %v", err)
	}
	return disabled
}

func TestEnterpriseRotationRefreshesPaidCatalogWhileDisabled(t *testing.T) {
	t.Run("disabled collection", func(t *testing.T) { testEnterpriseRotationRefresh(t, false) })
	t.Run("disabled account", func(t *testing.T) { testEnterpriseRotationRefresh(t, true) })
}

func testEnterpriseRotationRefresh(t *testing.T, isolated bool) {
	h, root, _ := enterpriseAccountFixture(t)
	if isolated {
		root = disableEnterpriseRotationFixture(t, h, root)
	}
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	target := enterpriseRotationTarget{
		Name: root.FileName, AuthIndex: lockedAuthIndex(root), AccountID: graph.ID,
		AuthorizationRevision: coreauth.EnterpriseOAuthAuthorizationRevision(root), Project: "123", Location: "us", Isolated: isolated,
	}
	_, before, err := h.enterpriseRotationState(target)
	if err != nil || len(before.Collections) != 1 || !before.Collections[0].Disabled || before.AccountDisabled != isolated {
		t.Fatalf("expected a disabled target: %v %+v", err, before)
	}
	request := enterpriseRotationRequest{enterpriseRotationTarget: target, ExpectedRevision: before.AccountRevision,
		BindingIDs: []string{before.Collections[0].BindingID}, Engines: []string{before.Collections[0].Engine},
		Owner: strings.Repeat("a", 32), Action: "hold"}
	hold := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseRotation, "PATCH", request)
	var state enterpriseRotationState
	if err := json.Unmarshal(hold.Body.Bytes(), &state); err != nil || hold.Code != http.StatusOK {
		t.Fatalf("could not hold the target: %s", hold.Body.String())
	}
	request.ExpectedRevision = state.AccountRevision
	previous := newGeminiEnterpriseRotationClient
	t.Cleanup(func() { newGeminiEnterpriseRotationClient = previous })
	read := map[string]int{}
	newGeminiEnterpriseRotationClient = func(_ string) (*http.Client, error) {
		return &http.Client{Transport: enterpriseRotationTransport(func(req *http.Request) (*http.Response, error) {
			read[req.URL.Path]++
			var body string
			switch req.URL.Path {
			case "/oauth2/v2/userinfo":
				body = `{"email":"account@example.test","verified_email":true}`
			case "/v1/projects/123/locations/us/userStores/default_user_store/userLicenses":
				body = `{"userLicenses":[{"userPrincipal":"account@example.test","licenseAssignmentState":"ASSIGNED","licenseConfig":"projects/123/locations/us/licenseConfigs/paid"}]}`
			case "/v1/projects/123/locations/us/licenseConfigs/paid":
				body = `{"name":"projects/123/locations/us/licenseConfigs/paid","subscriptionTier":"SEARCH_AND_ASSISTANT","state":"ACTIVE","freeTrial":false}`
			case "/v1alpha/" + state.Collections[0].Engine + "/widgetConfigs/default_search_widget_config":
				body = `{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"gemini-4-hydrogen","displayName":"Hydrogen"}]}}}`
			default:
				t.Fatalf("unexpected metadata path %s", req.URL.Path)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}, nil
	}
	refresh := enterpriseManagementRequest(t, h.RefreshGeminiEnterpriseRotation, "POST", request)
	const model = "gemini-4-hydrogen"
	if refresh.Code != http.StatusOK || !strings.Contains(refresh.Body.String(), model) || !strings.Contains(refresh.Body.String(), `"free_trial":false`) {
		t.Fatalf("refresh did not prove the catalog and paid license: %s", refresh.Body.String())
	}
	if len(read) != 4 {
		t.Fatalf("metadata read escaped its scope: %v", read)
	}
	_, after, err := h.enterpriseRotationState(target)
	if err != nil || after.HoldOwner != request.Owner || !after.Collections[0].Disabled || after.Collections[0].Routable ||
		after.AccountDisabled != isolated || (isolated && after.OutsideScopeRoutable) {
		t.Fatalf("refresh altered application admission: %v %+v", err, after)
	}
}

func TestEnterpriseIsolatedRotationRequiresExactModeIdentityAndEngineSet(t *testing.T) {
	h, root, _ := enterpriseAccountFixture(t)
	capabilities := enterpriseManagementRequest(t, h.GeminiEnterpriseRotationCapabilities, "GET", nil)
	if capabilities.Code != http.StatusOK || !strings.Contains(capabilities.Body.String(), `"isolated_disabled_account":true`) {
		t.Fatalf("missing isolated rotation capability: %s", capabilities.Body.String())
	}
	root = disableEnterpriseRotationFixture(t, h, root)
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	target := enterpriseRotationTarget{Name: root.FileName, AuthIndex: lockedAuthIndex(root), AccountID: graph.ID,
		AuthorizationRevision: coreauth.EnterpriseOAuthAuthorizationRevision(root), Project: "123", Location: "global", Isolated: true}
	read := func(target enterpriseRotationTarget, ensure bool) *httptest.ResponseRecorder {
		t.Helper()
		query := url.Values{"name": {target.Name}, "auth_index": {target.AuthIndex}, "account_id": {target.AccountID},
			"authorization_revision": {strconv.FormatUint(target.AuthorizationRevision, 10)}, "project": {target.Project},
			"location": {target.Location}, "isolated": {strconv.FormatBool(target.Isolated)}, "ensure_access": {strconv.FormatBool(ensure)},
			"engines": target.ExpectedEngines}
		router := gin.New()
		router.GET("/", h.GetGeminiEnterpriseRotation)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", "/?"+query.Encode(), nil))
		return response
	}
	patch := func(request enterpriseRotationRequest) *httptest.ResponseRecorder {
		t.Helper()
		return enterpriseManagementRequest(t, h.PatchGeminiEnterpriseRotation, "PATCH", request)
	}
	legacy := target
	legacy.Isolated = false
	if legacyRead := read(legacy, false); legacyRead.Code != http.StatusConflict {
		t.Fatal("the prior rotation mode accepted a disabled parent")
	}
	ensured := read(target, true)
	if ensured.Code != http.StatusOK || strings.Contains(ensured.Body.String(), "synthetic-private") {
		t.Fatalf("isolated access readback failed or returned credential material: %d", ensured.Code)
	}
	_, before, err := h.enterpriseRotationState(target)
	if err != nil || !before.AccountDisabled || before.OutsideScopeRoutable || len(before.Collections) != 1 {
		t.Fatalf("disabled account projection is wrong: %v %+v", err, before)
	}
	target.ExpectedEngines = []string{before.Collections[0].Engine}
	incorrect := target
	incorrect.ExpectedEngines = []string{before.Collections[0].Engine + "-other"}
	if wrongEngine := read(incorrect, true); wrongEngine.Code != http.StatusConflict {
		t.Fatal("a different Engine was accepted for the access check")
	}
	request := enterpriseRotationRequest{enterpriseRotationTarget: target, ExpectedRevision: before.AccountRevision,
		BindingIDs: []string{before.Collections[0].BindingID}, Engines: target.ExpectedEngines,
		Owner: strings.Repeat("a", 32), Action: "hold"}
	for _, test := range []struct {
		name   string
		change func(*enterpriseRotationRequest)
	}{
		{"normal mode", func(req *enterpriseRotationRequest) { req.Isolated = false }},
		{"other account", func(req *enterpriseRotationRequest) { req.AccountID += "-other" }},
		{"stale authorization", func(req *enterpriseRotationRequest) { req.AuthorizationRevision++ }},
		{"same binding with another Engine", func(req *enterpriseRotationRequest) { req.Engines = incorrect.ExpectedEngines }},
		{"absent Engine set", func(req *enterpriseRotationRequest) { req.Engines = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := request
			test.change(&changed)
			if response := patch(changed); response.Code != http.StatusConflict {
				t.Fatalf("slot escaped its scope: %s", response.Body.String())
			}
		})
	}
	requireState := func(response *httptest.ResponseRecorder) enterpriseRotationState {
		t.Helper()
		var state enterpriseRotationState
		if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil || response.Code != http.StatusOK {
			t.Fatalf("rotation operation failed: %s", response.Body.String())
		}
		return state
	}
	held := requireState(patch(request))
	request.ExpectedRevision, request.Action = held.AccountRevision, "retire"
	foreign := request
	foreign.Owner = strings.Repeat("b", 32)
	if response := patch(foreign); response.Code != http.StatusConflict {
		t.Fatal("a different job retired the source")
	}
	retired := requireState(patch(request))
	if !retired.AccountDisabled || !retired.Collections[0].Disabled || retired.OutsideScopeRoutable {
		t.Fatalf("retirement changed the disabled parent: %+v", retired)
	}

	target.Location, target.ExpectedEngines = "us", nil
	_, destination, err := h.enterpriseRotationState(target)
	if err != nil {
		t.Fatal(err)
	}
	request.enterpriseRotationTarget = target
	request.ExpectedRevision, request.Action = destination.AccountRevision, "hold"
	request.BindingIDs, request.Engines = []string{destination.Collections[0].BindingID}, []string{destination.Collections[0].Engine}
	held = requireState(patch(request))
	request.ExpectedRevision, request.Action = held.AccountRevision, "activate"
	activated := requireState(patch(request))
	if activated.AccountDisabled || activated.OutsideScopeRoutable || activated.Collections[0].RouteDisabled || activated.HoldOwner != "" {
		t.Fatalf("isolated activation left the wrong routing state: %+v", activated)
	}
	target.ExpectedEngines = request.Engines
	readback := requireState(read(target, false))
	if readback.AccountDisabled || readback.OutsideScopeRoutable {
		t.Fatalf("independent readback did not prove exclusive target routing: %+v", readback)
	}
	if response := read(target, true); response.Code != http.StatusConflict {
		t.Fatal("the isolated token acquisition path accepted an enabled parent")
	}
	if response := patch(request); response.Code != http.StatusConflict {
		t.Fatal("the isolated mutation path accepted an enabled parent")
	}
}

func TestEnterpriseIsolatedRotationRefreshesExpiredAccessWithoutEnablingParent(t *testing.T) {
	h, root, _ := enterpriseAccountFixture(t)
	root.Metadata["expired"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	root = disableEnterpriseRotationFixture(t, h, root)
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	query := url.Values{"name": {root.FileName}, "auth_index": {lockedAuthIndex(root)}, "account_id": {graph.ID},
		"authorization_revision": {strconv.FormatUint(coreauth.EnterpriseOAuthAuthorizationRevision(root), 10)},
		"project":                {"123"}, "location": {"us"}, "isolated": {"true"}, "ensure_access": {"true"}}
	router := gin.New()
	router.GET("/", h.GetGeminiEnterpriseRotation)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/?"+query.Encode(), nil))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "credential_unavailable") ||
		strings.Contains(response.Body.String(), "synthetic-private") {
		t.Fatalf("wrong diagnostic for an unavailable credential: %s", response.Body.String())
	}
	const refreshedAccess = "synthetic-private-refreshed"
	h.authManager.RegisterExecutor(&enterpriseRotationRefreshExecutor{refresh: func(auth *coreauth.Auth) (*coreauth.Auth, error) {
		if auth.ID != root.ID || !auth.Disabled {
			t.Error("the refresh passed an enabled or unrelated credential")
		}
		auth.Metadata["access_token"] = refreshedAccess
		auth.Metadata["expired"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
		return auth, nil
	}})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/?"+query.Encode(), nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"account_disabled":true`) ||
		strings.Contains(response.Body.String(), "synthetic-private") {
		t.Fatalf("refresh did not preserve isolated state: %s", response.Body.String())
	}
	current, _, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	if current == nil || !current.Disabled || ge.String(current.Metadata, "access_token") != refreshedAccess {
		t.Fatal("new access was not saved on the disabled parent")
	}
}

func TestEnterpriseIsolatedRotationCannotReuseANormalHold(t *testing.T) {
	h, root, _ := enterpriseAccountFixture(t)
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	target := enterpriseRotationTarget{Name: root.FileName, AuthIndex: lockedAuthIndex(root), AccountID: graph.ID,
		AuthorizationRevision: coreauth.EnterpriseOAuthAuthorizationRevision(root), Project: "123", Location: "global"}
	_, before, err := h.enterpriseRotationState(target)
	if err != nil {
		t.Fatal(err)
	}
	request := enterpriseRotationRequest{enterpriseRotationTarget: target, ExpectedRevision: before.AccountRevision,
		BindingIDs: []string{before.Collections[0].BindingID}, Engines: []string{before.Collections[0].Engine},
		Owner: strings.Repeat("a", 32), Action: "hold"}
	response := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseRotation, "PATCH", request)
	var held enterpriseRotationState
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &held) != nil {
		t.Fatalf("normal hold failed: %s", response.Body.String())
	}
	current, _, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	disableEnterpriseRotationFixture(t, h, current)
	request.ExpectedRevision, request.Isolated, request.Action = held.AccountRevision, true, "activate"
	response = enterpriseManagementRequest(t, h.PatchGeminiEnterpriseRotation, "PATCH", request)
	if response.Code != http.StatusConflict {
		t.Fatal("the normal hold was reused as an isolated activation grant")
	}
	current, saved, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	if !current.Disabled || saved.RotationHolds["123/global"] != request.Owner {
		t.Fatal("the cross-mode request altered the parent or hold")
	}
}

func TestEnterpriseRotationHoldFencesScopeAndOwner(t *testing.T) {
	h, root, children := enterpriseAccountFixture(t)
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	target := enterpriseRotationTarget{
		Name: root.FileName, AuthIndex: lockedAuthIndex(root), AccountID: graph.ID,
		AuthorizationRevision: coreauth.EnterpriseOAuthAuthorizationRevision(root), Project: "123", Location: "global",
	}
	_, original, err := h.enterpriseRotationState(target)
	if err != nil || len(original.Collections) != 1 {
		t.Fatalf("rotation state: %v, %+v", err, original)
	}
	owner := strings.Repeat("a", 32)
	request := enterpriseRotationRequest{enterpriseRotationTarget: target, ExpectedRevision: original.AccountRevision,
		BindingIDs: []string{original.Collections[0].BindingID}, Owner: owner, Action: "hold"}
	resp := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseRotation, "PATCH", request)
	if resp.Code != http.StatusOK {
		t.Fatalf("hold failed: %s", resp.Body.String())
	}
	var held enterpriseRotationState
	if err := json.Unmarshal(resp.Body.Bytes(), &held); err != nil || held.HoldOwner != owner ||
		!held.Collections[0].RouteDisabled || held.Collections[0].Disabled || held.Collections[0].Routable {
		t.Fatalf("hold changed the manual switch or left routing active: %s", resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "synthetic-private") {
		t.Fatal("rotation exposed OAuth material")
	}
	persisted, snapshot, ok := h.authManager.EnterpriseAccountSnapshot(root.ID)
	if !ok || snapshot.RotationHolds["123/global"] != owner {
		t.Fatal("hold missing from saved graph")
	}
	projected, err := coreauth.ProjectEnterpriseAuth(persisted)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range projected {
		if coreauth.EnterpriseBindingID(child) == request.BindingIDs[0] && !child.Disabled {
			t.Fatal("hold did not survive projection from saved metadata")
		}
	}
	for _, child := range children {
		if ge.FromMetadata(child.Metadata).Location != "global" {
			current, _ := h.authManager.GetByID(child.ID)
			if current.Disabled != child.Disabled {
				t.Fatal("the hold changed a sibling region")
			}
		}
	}
	request.Action = "retire"
	if stale := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseRotation, "PATCH", request); stale.Code != http.StatusConflict {
		t.Fatal("the pre-hold account revision was accepted")
	}
	request.ExpectedRevision = held.AccountRevision
	request.Owner = strings.Repeat("b", 32)
	if foreign := enterpriseManagementRequest(t, h.PatchGeminiEnterpriseRotation, "PATCH", request); foreign.Code != http.StatusConflict {
		t.Fatal("a foreign rotation task removed the hold")
	}
	request.Owner = owner
	resp = enterpriseManagementRequest(t, h.PatchGeminiEnterpriseRotation, "PATCH", request)
	var retired enterpriseRotationState
	if err := json.Unmarshal(resp.Body.Bytes(), &retired); err != nil || resp.Code != http.StatusOK || retired.HoldOwner != "" ||
		!retired.Collections[0].Disabled || !retired.Collections[0].RouteDisabled {
		t.Fatalf("retirement did not safely release the hold: %s", resp.Body.String())
	}
}
