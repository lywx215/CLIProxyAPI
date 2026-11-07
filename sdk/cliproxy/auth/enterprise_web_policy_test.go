package auth

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type webPolicySelectionExecutor struct {
	ProviderExecutor
	seen      []string
	failFirst bool
}

func (*webPolicySelectionExecutor) Identifier() string { return web.Provider }
func (e *webPolicySelectionExecutor) Execute(_ context.Context, a *Auth, req exec.Request, _ exec.Options) (exec.Response, error) {
	c, _ := web.CredentialFrom(a.Metadata)
	if !c.AllowsModel(req.Model) {
		return exec.Response{}, fmt.Errorf("wrong policy selected: %s for %s", a.ID, req.Model)
	}
	e.seen = append(e.seen, a.ID)
	if e.failFirst && len(e.seen) == 1 {
		return exec.Response{}, &Error{Code: "synthetic", Message: "before submission", HTTPStatus: 500}
	}
	return exec.Response{Payload: []byte(`{"candidates":[{"finishReason":"STOP"}]}`)}, nil
}

func registerWebPolicyAccount(t *testing.T, m *Manager, id, policy string, priority int, prefix string) *Auth {
	t.Helper()
	c := web.Credential{ModelPolicy: policy, Revision: 1}
	if err := c.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/test"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCookies("SID=synthetic-" + id + "; SAPISID=synthetic-signing"); err != nil {
		t.Fatal(err)
	}
	c.SetCatalog([]web.AvailableModel{{ID: web.ArgonModel}, {ID: "gemini-3.8-flash"}}, time.Now())
	a, err := m.Register(context.Background(), &Auth{ID: id, Provider: web.Provider, Prefix: prefix, Status: StatusActive, Metadata: map[string]any{"enterprise_web": c.MetadataValue(), "priority": priority}, Attributes: map[string]string{"priority": fmt.Sprint(priority)}})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately over-broad registry state proves selection does not trust stale publication.
	models := []*registry.ModelInfo{{ID: web.ArgonModel}, {ID: "gemini-3.8-flash"}, {ID: "argon-public"}, {ID: "flash-public"}}
	if prefix != "" {
		for _, model := range models {
			model.ID = prefix + "/" + model.ID
		}
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, web.Provider, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	return a
}

func webPolicyOptions() exec.Options {
	return exec.Options{SourceFormat: translator.Format("gemini"), ResponseFormat: translator.Format("gemini"), Metadata: map[string]any{exec.OperationMetadataKey: "generate_content"}}
}
func webPolicyRequest(model string) exec.Request {
	return exec.Request{Model: model, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"read"},{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}]}]}`)}
}

func TestEnterpriseWebManualPoliciesSelectCorrectPoolForAllSelectors(t *testing.T) {
	testEnterpriseWebManualPoliciesSelectCorrectPool(t, false)
}

func TestEnterpriseWebSerializedAliasesSelectCorrectPool(t *testing.T) {
	testEnterpriseWebManualPoliciesSelectCorrectPool(t, true)
}

func testEnterpriseWebManualPoliciesSelectCorrectPool(t *testing.T, serialized bool) {
	for _, selector := range []Selector{&RoundRobinSelector{}, &FillFirstSelector{}, &WeightedRoundRobinSelector{}} {
		t.Run(fmt.Sprintf("%T", selector), func(t *testing.T) {
			m := NewManager(nil, selector, nil)
			cfg := &config.Config{}
			cfg.GeminiEnterpriseWeb.Enabled = true
			if serialized {
				cfg.GeminiEnterpriseWeb.HistoryMode = config.EnterpriseWebSerializedContext
			}
			m.SetConfig(cfg)
			e := &webPolicySelectionExecutor{}
			m.RegisterExecutor(e)
			m.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{web.Provider: {{Name: web.ArgonModel, Alias: "argon-public", Fork: true}, {Name: "gemini-3.8-flash", Alias: "flash-public", Fork: true}}})
			registerWebPolicyAccount(t, m, "pending", "", 100, "")
			registerWebPolicyAccount(t, m, "argon-a", web.PolicyArgonOnly, 1, "p")
			registerWebPolicyAccount(t, m, "argon-b", web.PolicyArgonOnly, 1, "p")
			registerWebPolicyAccount(t, m, "nonargon", web.PolicyExcludeArgon, 100, "p")
			for _, model := range []string{"p/argon-public(8192)", "p/" + web.ArgonModel, "p/flash-public", "p/gemini-3.8-flash(8192)"} {
				req := webPolicyRequest(model)
				if serialized {
					req.Payload = []byte(strings.TrimSuffix(string(req.Payload), "]}") + `,{"role":"model","parts":[{"text":"visible history"}]},{"role":"user","parts":[{"text":"follow up on the original attachment"}]}]}`)
				}
				for i := 0; i < 4; i++ {
					if _, err := m.Execute(context.Background(), []string{web.Provider}, req, webPolicyOptions()); err != nil {
						t.Fatal(model, err)
					}
				}
			}
			if len(e.seen) != 16 {
				t.Fatal(e.seen)
			}
			for i, id := range e.seen {
				if (i < 8 && !strings.HasPrefix(id, "argon-")) || (i >= 8 && id != "nonargon") {
					t.Fatal("cross-pool selection", e.seen)
				}
			}
			if _, ok := selector.(*RoundRobinSelector); ok && e.seen[0] == e.seen[1] {
				t.Fatal("round robin did not rotate within policy pool", e.seen)
			}
		})
	}
}

func TestEnterpriseWebPendingPinnedAndRetryNeverCrossPolicies(t *testing.T) {
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	cfg := &config.Config{}
	cfg.GeminiEnterpriseWeb.Enabled = true
	m.SetConfig(cfg)
	e := &webPolicySelectionExecutor{}
	m.RegisterExecutor(e)
	registerWebPolicyAccount(t, m, "pending-policy", "", 1, "")
	_, err := m.Execute(context.Background(), []string{web.Provider}, webPolicyRequest(web.ArgonModel), webPolicyOptions())
	if err == nil || !strings.Contains(err.Error(), "manually classify") || len(e.seen) != 0 || m.WebRuntime().Status()["active_requests"] != 0 {
		t.Fatal("pending generated or leaked admission", err, e.seen, m.WebRuntime().Status())
	}
	a := registerWebPolicyAccount(t, m, "retry-argon-a", web.PolicyArgonOnly, 1, "")
	registerWebPolicyAccount(t, m, "retry-argon-b", web.PolicyArgonOnly, 1, "")
	registerWebPolicyAccount(t, m, "retry-other", web.PolicyExcludeArgon, 100, "")
	opts := webPolicyOptions()
	opts.Metadata[exec.PinnedAuthMetadataKey] = a.ID
	_, err = m.Execute(context.Background(), []string{web.Provider}, webPolicyRequest("gemini-3.8-flash"), opts)
	if err == nil || !strings.Contains(err.Error(), "does not allow") || len(e.seen) != 0 {
		t.Fatal("pinned policy bypass", err, e.seen)
	}
	m.SetRetryConfig(2, 0, 0)
	e.failFirst = true
	if _, err = m.Execute(context.Background(), []string{web.Provider}, webPolicyRequest(web.ArgonModel), webPolicyOptions()); err != nil {
		t.Fatal(err)
	}
	if len(e.seen) != 2 || e.seen[0] == e.seen[1] || !strings.HasPrefix(e.seen[0], "retry-argon-") || !strings.HasPrefix(e.seen[1], "retry-argon-") {
		t.Fatal("retry left its class", e.seen)
	}
}

func TestEnterpriseWebPolicySelectionAlsoHonorsGlobalExclusions(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := &config.Config{}
	cfg.GeminiEnterpriseWeb.Enabled = true
	cfg.OAuthExcludedModels = map[string][]string{web.Provider: {"gemini-4-*"}}
	m.SetConfig(cfg)
	e := &webPolicySelectionExecutor{}
	m.RegisterExecutor(e)
	a := registerWebPolicyAccount(t, m, "excluded-argon", web.PolicyArgonOnly, 1, "")
	_, err := m.Execute(context.Background(), []string{web.Provider}, webPolicyRequest(web.ArgonModel), webPolicyOptions())
	if err == nil || len(e.seen) != 0 {
		t.Fatal("global exclusion bypassed selection", err, e.seen)
	}
	credential, _ := web.CredentialFrom(a.Metadata)
	if err = m.validateEnterpriseWebDispatchPolicy(a.ID, credential, web.ArgonModel); err == nil {
		t.Fatal("global exclusion bypassed final dispatch")
	}
}

func TestEnterpriseWebPolicyReloadRetainsCatalogAndFencesOldResults(t *testing.T) {
	store := newMemoryAuthTestStore()
	m := NewManager(store, nil, nil)
	a := registerWebPolicyAccount(t, m, "policy-reload", web.PolicyExcludeArgon, 1, "")
	c, _ := web.CredentialFrom(a.Metadata)
	authEpoch, evidenceEpoch := m.WebRuntime().CredentialEpoch(a.ID), m.WebRuntime().EvidenceEpoch(a.ID)
	m.WebRuntime().RecordModel(a.ID, a.Metadata, "gemini-3.8-flash", evidenceEpoch)
	// Simulate a watcher source edit without requiring a wall-clock filesystem delay.
	c.ModelPolicy = web.PolicyArgonOnly
	a.Metadata["enterprise_web"] = c.MetadataValue()
	if _, err := store.Save(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := m.GetByID(a.ID)
	if !m.enterpriseWebAuthAllowsModel(current, web.ArgonModel) || m.enterpriseWebAuthAllowsModel(current, "gemini-3.8-flash") {
		t.Fatal("reload retained old policy")
	}
	if !m.EnterpriseWebModel("gemini-3.8-flash") || m.WebRuntime().CredentialEpoch(a.ID) != authEpoch || m.WebRuntime().EvidenceEpoch(a.ID) == evidenceEpoch {
		t.Fatal("reload lost routing/catalog or authentication separation")
	}
	m.WebRuntime().RecordModel(a.ID, a.Metadata, "late", evidenceEpoch)
	if len(m.WebRuntime().CredentialStatus(a.ID, c)["tested_model_ids"].([]string)) != 0 {
		t.Fatal("reload accepted an old result")
	}
}
