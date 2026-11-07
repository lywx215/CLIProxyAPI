package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/tidwall/gjson"
)

func TestEnterpriseWebManualPolicyAPIAndLegacyPending(t *testing.T) {
	h, r := webManagementFixture(t)
	input := map[string]any{"label": "policy-test", "page_url": "https://vertexaisearch.cloud.google.com/ca/home/cid/test", "cookies": "SID=synthetic-policy; SAPISID=synthetic-signing"}
	for _, policy := range []any{nil, "auto", "catalog", "custom", "unclassified", "", 42} {
		if policy == nil {
			delete(input, "model_policy")
		} else {
			input["model_policy"] = policy
		}
		w := webManagementRequest(t, r, "POST", "/v8/management/credentials/gemini-enterprise-web", input, nil)
		if w.Code != 400 {
			t.Fatal("accepted missing/invalid classification", policy, w.Code)
		}
	}
	name := addWebManagementCredential(t, r)
	for _, invalid := range []any{nil, "auto", "catalog", "custom", "unclassified", "", 42} {
		w := webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{"name": name, "expected_revision": 1, "model_policy": invalid}, nil)
		if w.Code != 400 {
			t.Fatal("invalid classification edit", invalid, w.Code)
		}
	}
	a, _ := h.authManager.GetByID(name)
	c, _ := web.CredentialFrom(a.Metadata)
	c.ModelPolicy = ""
	c.Revision++
	c.SetCatalog([]web.AvailableModel{{ID: web.ArgonModel}, {ID: "gemini-3.8-flash"}}, time.Now())
	a.Metadata["enterprise_web"] = c.MetadataValue()
	if _, err := h.authManager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	list := webManagementRequest(t, r, "GET", "/v8/management/credentials", nil, nil)
	if gjson.GetBytes(list.Body.Bytes(), "files.0.gemini_enterprise_web.model_policy").String() != web.PolicyUnclassified || gjson.GetBytes(list.Body.Bytes(), "files.0.gemini_enterprise_web.effective_model_ids.#").Int() != 0 || gjson.GetBytes(list.Body.Bytes(), "files.0.gemini_enterprise_web.available_models.#").Int() != 2 {
		t.Fatal(list.Body.String())
	}
	epoch := h.authManager.WebRuntime().CredentialEpoch(a.ID)
	for i, policy := range []string{web.PolicyArgonOnly, web.PolicyExcludeArgon} {
		w := webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{"name": name, "expected_revision": i + 2, "model_policy": policy}, nil)
		if w.Code != 200 || gjson.GetBytes(w.Body.Bytes(), "credential.gemini_enterprise_web.model_policy").String() != policy || gjson.GetBytes(w.Body.Bytes(), "credential.gemini_enterprise_web.effective_model_ids.#").Int() != 1 || gjson.GetBytes(w.Body.Bytes(), "credential.gemini_enterprise_web.available_models.#").Int() != 2 {
			t.Fatal(w.Code, w.Body.String())
		}
		updated, _ := h.authManager.GetByID(a.ID)
		now, _ := web.CredentialFrom(updated.Metadata)
		if now.Fingerprint() != c.Fingerprint() || h.authManager.WebRuntime().CredentialEpoch(a.ID) != epoch {
			t.Fatal("policy edit invalidated authentication")
		}
	}
	w := webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{"name": name, "expected_revision": 4, "cookies": "SID=replaced-policy; SAPISID=synthetic-signing"}, nil)
	if w.Code != 200 || gjson.GetBytes(w.Body.Bytes(), "credential.gemini_enterprise_web.model_policy").String() != web.PolicyExcludeArgon {
		t.Fatal("replacement lost classification", w.Code, w.Body.String())
	}
	// Detect a manual source edit even if its author did not advance the persisted revision.
	path := filepath.Join(h.cfg.AuthDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if json.Unmarshal(data, &metadata) != nil {
		t.Fatal("invalid test credential")
	}
	changed, _ := web.CredentialFrom(metadata)
	changed.ModelPolicy = web.PolicyArgonOnly
	metadata["enterprise_web"] = changed.MetadataValue()
	data, _ = json.Marshal(metadata)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	w = webManagementRequest(t, r, "PATCH", "/v8/management/credentials/gemini-enterprise-web", map[string]any{"name": name, "expected_revision": 5, "label": "stale-source"}, nil)
	if w.Code != 409 {
		t.Fatal("stale file edit overwrote classification", w.Code, w.Body.String())
	}
}

func TestEnterpriseWebModelProbeRejectsManualPolicyBeforeAnyGoogleCall(t *testing.T) {
	for _, policy := range []string{"", web.PolicyArgonOnly} {
		h, r := webManagementFixture(t)
		name := addWebManagementCredential(t, r)
		a, _ := h.authManager.GetByID(name)
		c, _ := web.CredentialFrom(a.Metadata)
		c.ModelPolicy = policy
		c.Revision++
		c.SetCatalog([]web.AvailableModel{{ID: "gemini-3.8-flash"}}, time.Now())
		a.Metadata["enterprise_web"] = c.MetadataValue()
		if _, err := h.authManager.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		h.cfg.GeminiEnterpriseWeb.Enabled = true
		h.authManager.SetConfig(h.cfg)
		request := map[string]any{"name": name, "auth_index": lockedAuthIndex(a), "model": "gemini-3.8-flash", "expected_upstream_model": "gemini-3.8-flash", "expected_revision": c.Revision}
		w := webManagementRequest(t, r, "POST", "/v8/management/credentials/gemini-enterprise-web/models/test", request, nil)
		want := 400
		code := "model_policy_denied"
		if policy == "" {
			want = 409
			code = "model_policy_unclassified"
		}
		if w.Code != want || gjson.GetBytes(w.Body.Bytes(), "code").String() != code || h.authManager.WebRuntime().Status()["active_requests"] != 0 {
			t.Fatal("policy bypassed", policy, w.Code, w.Body.String())
		}
	}
}
