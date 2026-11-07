package cliproxy

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEnterpriseWebModelsUseObservedCatalogAndGlobalAliases(t *testing.T) {
	c := web.Credential{ModelPolicy: web.PolicyExcludeArgon, Revision: 1}
	if err := c.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/test"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCookies("SID=synthetic; SAPISID=synthetic"); err != nil {
		t.Fatal(err)
	}
	c.SetCatalog([]web.AvailableModel{{ID: "gemini-3.5-flash", DisplayName: "Gemini Flash"}}, time.Now())
	a := &auth.Auth{ID: "catalog-account", Provider: web.Provider, Metadata: map[string]any{"enterprise_web": c.MetadataValue()}}
	auth.SetOAuthModelAliasesAttribute(a, []config.OAuthModelAlias{{Name: "gemini-3.5-flash", Alias: "credential-alias"}})
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{web.Provider: {{Name: "gemini-3.5-flash", Alias: "shared-alias", Fork: false}}}}
	models := ProjectEnterpriseWebModels(cfg, a)
	if len(models) != 1 || models[0].ID != "shared-alias" || models[0].MetadataModelID != "gemini-3.5-flash" {
		t.Fatal("global alias projection failed", models)
	}
	native := EnterpriseWebNativeModels(a)
	if len(native) != 1 || native[0].ID != "gemini-3.5-flash" {
		t.Fatal("upstream definition became an alias")
	}
	cfg.OAuthExcludedModels = map[string][]string{web.Provider: {"gemini-3.5-*"}}
	if len(ProjectEnterpriseWebModels(cfg, a)) != 0 {
		t.Fatal("excluded model still available")
	}
	if err := c.SetCookies("SID=replaced; SAPISID=synthetic"); err != nil {
		t.Fatal(err)
	}
	a.Metadata["enterprise_web"] = c.MetadataValue()
	if len(EnterpriseWebNativeModels(a)) != 0 {
		t.Fatal("replacement retained another Cookie's catalog")
	}
}

func TestEnterpriseWebProjectionFiltersClassesButRetainsUpstreamCatalog(t *testing.T) {
	c := web.Credential{Revision: 1}
	if err := c.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/test"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCookies("SID=synthetic; SAPISID=synthetic"); err != nil {
		t.Fatal(err)
	}
	c.SetCatalog([]web.AvailableModel{{ID: web.ArgonModel}, {ID: "gemini-3.8-flash"}}, time.Now())
	a := &auth.Auth{ID: "web-policy-projection", Provider: web.Provider, Metadata: map[string]any{}, Prefix: "web"}
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{web.Provider: {{Name: web.ArgonModel, Alias: "argon-public", Fork: false}, {Name: "gemini-3.8-flash", Alias: "flash-public", Fork: false}}}}
	for _, policy := range []string{"", web.PolicyArgonOnly, web.PolicyExcludeArgon} {
		c.ModelPolicy = policy
		a.Metadata["enterprise_web"] = c.MetadataValue()
		models := ProjectEnterpriseWebModels(cfg, a)
		if policy == "" {
			if len(models) != 0 {
				t.Fatal("pending registered models", models)
			}
		} else {
			if len(models) != 2 {
				t.Fatal("prefix projection", policy, models)
			}
			want := "flash-public"
			if policy == web.PolicyArgonOnly {
				want = "argon-public"
			}
			for _, model := range models {
				if model.ID != want && model.ID != "web/"+want {
					t.Fatal("wrong policy advertised", model.ID)
				}
			}
		}
		if len(c.Catalog()) != 2 {
			t.Fatal("classification removed upstream definitions")
		}
	}
}
