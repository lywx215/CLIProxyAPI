package management

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type enterpriseRotationTransport func(*http.Request) (*http.Response, error)

func (t enterpriseRotationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t(request)
}

func TestEnterpriseRotationRefreshesPaidCatalogWhileDisabled(t *testing.T) {
	h, root, _ := enterpriseAccountFixture(t)
	_, graph, _ := h.authManager.EnterpriseAccountSnapshot(root.ID)
	target := enterpriseRotationTarget{
		Name: root.FileName, AuthIndex: lockedAuthIndex(root), AccountID: graph.ID,
		AuthorizationRevision: coreauth.EnterpriseOAuthAuthorizationRevision(root), Project: "123", Location: "us",
	}
	_, before, err := h.enterpriseRotationState(target)
	if err != nil || len(before.Collections) != 1 || !before.Collections[0].Disabled {
		t.Fatalf("expected a disabled target: %v %+v", err, before)
	}
	request := enterpriseRotationRequest{enterpriseRotationTarget: target, ExpectedRevision: before.AccountRevision,
		BindingIDs: []string{before.Collections[0].BindingID}, Owner: strings.Repeat("a", 32), Action: "hold"}
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
	if err != nil || after.HoldOwner != request.Owner || !after.Collections[0].Disabled || after.Collections[0].Routable {
		t.Fatalf("refresh altered application admission: %v %+v", err, after)
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
