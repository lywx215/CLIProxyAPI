package antigravityenterprise

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWeeklyQuotaPresenceAndSubscriptionGate(t *testing.T) {
	now := time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC)
	cases := []struct{ name, body, code string }{
		{"remaining", `{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":1}]}],"overageInfo":{}}`, ""},
		{"zero", `{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingFraction":0}]}],"overageInfo":{}}`, "weekly_quota"},
		{"missing", `{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly"}]}],"overageInfo":{}}`, "quota_unknown"},
		{"amount", `{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingAmount":"42"}]}],"overageInfo":{}}`, ""},
		{"ambiguous", `{"groups":[{"buckets":[{"bucketId":"gemini-enterprise-weekly","window":"weekly","remainingAmount":"42","remainingFraction":1}]}],"overageInfo":{}}`, "quota_unknown"},
		{"payg", `{"overageInfo":{"enabled":true}}`, "subscription_only_required"},
		{"unknown_overage", `{}`, "subscription_only_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var q Quota
			if err := json.Unmarshal([]byte(tc.body), &q); err != nil {
				t.Fatal(err)
			}
			q.ObservedAt = now
			err := q.Admit(now)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			e, ok := err.(*Error)
			if !ok || e.Code != tc.code {
				t.Fatalf("got %v; want %s", err, tc.code)
			}
			if tc.code == "weekly_quota" && !e.RetryAt.IsZero() {
				t.Fatal("invented missing reset time")
			}
		})
	}
}
func TestDiscoveryPreservesDisabledAndIncompleteCoverage(t *testing.T) {
	l := License{ProjectID: "example-project", Location: "global", UserTier: "gcp-ge-plus-tier"}
	a := Account{Version: 1, ID: AccountID("subject", "client"), Revision: 1}
	a, err := MergeLicenses(a, []License{l}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a.Bindings[0].Disabled = true
	a.Bindings[0].Settings = map[string]any{"prefix": "keep"}
	us := l
	us.Location = "us"
	a, err = MergeLicenses(a, []License{us}, []string{"global"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Bindings) != 2 {
		t.Fatal("regions collapsed")
	}
	for _, v := range a.Bindings {
		if v.License.Location == "global" && (!v.Disabled || v.Missing || v.Settings["prefix"] != "keep") {
			t.Fatalf("lost retained settings: %+v", v)
		}
	}
	a, err = MergeLicenses(a, []License{us}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range a.Bindings {
		if v.License.Location == "global" && !v.Missing {
			t.Fatal("complete removal not marked missing")
		}
	}
}
func TestLicenseConflictIsNotAnotherSubscription(t *testing.T) {
	l := License{ProjectID: "example", Location: "global", UserTier: "gcp-ge-plus-tier"}
	other := l
	other.UserTier = "different-tier"
	a := Account{Version: 1, ID: "account", Revision: 1}
	if _, err := MergeLicenses(a, []License{l, other}, nil, time.Now()); err == nil {
		t.Fatal("conflicting tiers silently created multiple bindings")
	}
}
func TestDefaultCatalogAndConfigFiltering(t *testing.T) {
	if len(Models(Config{})) != 14 {
		t.Fatal("default directory must contain 14 options")
	}
	if len(Models(Config{AllowedModels: []string{"gemini-3.1-pro-preview"}})) != 2 {
		t.Fatal("model restrictions ignored")
	}
	for _, m := range Models(Config{}) {
		if m.ID == CarbonModel {
			t.Fatal("unlisted Carbon admitted by default")
		}
		if m.Upstream != m.ID || m.Upstream != m.Family+"-"+m.Level || m.Canonical == "" {
			t.Fatalf("native virtual ID lost: %+v", m)
		}
	}
	for _, tc := range []struct {
		id   string
		want int
	}{{"gemini-3.8-flash", 3}, {"gemini-3.8-flash-low", 1}, {"gemini-3.1-pro", 2}, {"gemini-3.1-pro-preview", 2}, {"gemini-3.1-pro-high", 1}, {CarbonModel, 0}} {
		if models := Models(Config{AllowedModels: []string{tc.id}}); len(models) != tc.want {
			t.Fatalf("allowlist %s: got %d models; want %d", tc.id, len(models), tc.want)
		}
	}
	experience := Experience{Name: "native-experience"}
	experience.Model.CanonicalID = "gemini-3.8-flash"
	experience.Model.ThinkingLevels = []string{"THINKING_LEVEL_LOW"}
	models := Models(Config{Experiences: []Experience{experience}})
	if len(models) != 1 || models[0].ID != "gemini-3.8-flash-low" || models[0].Upstream != "gemini-3.8-flash-low" || models[0].Experience != experience.Name {
		t.Fatalf("experience metadata changed the native wire ID: %+v", models)
	}
}
func TestErrorStatusAndDelay(t *testing.T) {
	e := DecodeError(200, nil, []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.5s"}]}}`), true)
	if e.HTTP != 429 || e.RetryAt.IsZero() || !e.IsRequestScoped() {
		t.Fatalf("stream error lost: %+v", e)
	}
	admin := DecodeError(403, nil, []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","domain":"businessaicode.googleapis.com","reason":"MODEL_NOT_ALLOWED_BY_ADMIN"}]}}`), false)
	if admin.Code != "model_not_enabled" || admin.Scope != "model" {
		t.Fatalf("model policy denial became binding-wide permission failure: %+v", admin)
	}
}

func TestDiscoveryOlderStartCannotRemoveNewerBindings(t *testing.T) {
	license := License{ProjectID: "project", Location: "global", UserTier: "gcp-ge-plus-tier"}
	newer := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	account := Account{Version: 1, ID: AccountID("subject", "client"), Revision: 1}
	account, err := MergeLicenses(account, []License{license}, nil, newer)
	if err != nil {
		t.Fatal(err)
	}
	revision := account.Revision
	late, err := MergeLicenses(account, nil, nil, newer.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if late.Revision != revision || len(late.Bindings) != 1 || late.Bindings[0].Missing {
		t.Fatal("old discovery deleted a newer binding")
	}
}

func TestAccountGraphRequiresStoredGoogleIdentity(t *testing.T) {
	metadata := PutAccount(nil, Account{Version: 1, ID: AccountID("", ""), Revision: 1})
	if _, err := ReadAccount(metadata); err == nil {
		t.Fatal("identity-free graph accepted")
	}
}
