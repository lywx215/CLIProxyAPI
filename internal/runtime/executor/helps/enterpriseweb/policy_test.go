package enterpriseweb

import (
	"context"
	"testing"
	"time"
)

func TestManualPolicyNeverInfersFromCatalogAndDoesNotChangeAuthentication(t *testing.T) {
	c := credentialFixture(t)
	c.ModelPolicy = ""
	c.SetCatalog([]AvailableModel{{ID: ArgonModel}, {ID: "gemini-3.8-flash"}}, time.Now())
	fingerprint := c.Fingerprint()
	if c.Policy() != PolicyUnclassified || c.Summary()["model_policy"] != PolicyUnclassified || c.CheckModelPolicy(ArgonModel) == nil || c.CheckModelPolicy("gemini-3.8-flash") == nil {
		t.Fatal("catalog inferred a manual policy")
	}
	for _, policy := range []string{PolicyArgonOnly, PolicyExcludeArgon} {
		c.ModelPolicy = policy
		if c.Validate() != nil || c.Fingerprint() != fingerprint || len(c.Catalog()) != 2 {
			t.Fatal("classification modified authentication or upstream catalog")
		}
		if c.AllowsModel(ArgonModel+"(8192)") != (policy == PolicyArgonOnly) || c.AllowsModel("gemini-3.8-flash") != (policy == PolicyExcludeArgon) || c.AllowsModel("Auto") {
			t.Fatal("incorrect classification", policy)
		}
	}
	for _, invalid := range []string{"auto", "catalog", "custom", PolicyUnclassified, "other"} {
		c.ModelPolicy = invalid
		if c.Validate() == nil {
			t.Fatal("accepted unsupported manual policy", invalid)
		}
	}
}

func TestPolicyRevisionsDoNotReuseLateModelEvidence(t *testing.T) {
	r := NewRuntime()
	c := credentialFixture(t)
	c.ModelPolicy, c.Revision = PolicyExcludeArgon, 1
	r.RecordModel("account", map[string]any{"enterprise_web": c.MetadataValue()}, "flash")
	old := c
	c.ModelPolicy, c.Revision = PolicyArgonOnly, 2
	r.RecordModel("account", map[string]any{"enterprise_web": old.MetadataValue()}, "late")
	if len(r.CredentialStatus("account", c)["tested_model_ids"].([]string)) != 0 {
		t.Fatal("old classification evidence appeared in a new revision")
	}
	c.ModelPolicy, c.Revision = PolicyExcludeArgon, 3
	if len(r.CredentialStatus("account", c)["tested_model_ids"].([]string)) != 0 {
		t.Fatal("switching back restored old evidence")
	}
	called := false
	ctx := WithPolicyValidator(context.Background(), func(_ string, _ Credential, _ string) error { called = true; return Fail(409, "changed") })
	if code(ValidateModelPolicy(ctx, "account", c, "gemini-3.8-flash")) != 409 || !called {
		t.Fatal("live dispatch check was bypassed")
	}
}
