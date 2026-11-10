package management

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	"github.com/tidwall/gjson"
)

func TestEnterpriseWebHistoryManagementConfigAndRuntime(t *testing.T) {
	h, router := webManagementFixture(t)
	patch := func(body string, want int) {
		t.Helper()
		response := webManagementRequest(t, router, "PATCH", "/v8/management/config/upstream/gemini-enterprise-web", body, nil)
		if response.Code != want {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	status := func() string {
		t.Helper()
		response := webManagementRequest(t, router, "GET", "/v8/management/credentials/gemini-enterprise-web/status", nil, nil)
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
		return response.Body.String()
	}
	if gjson.Get(status(), "history_mode").String() != "single-turn" || len(gjson.Get(status(), "supported_history_modes").Array()) != 2 {
		t.Fatal("default or advertised modes missing")
	}
	for _, reason := range []string{"web_disabled", "no_web_credential"} {
		counter := gjson.Get(status(), "media_fallback_requests."+reason)
		if counter.Type != gjson.Number || counter.Int() != 0 {
			t.Fatal("status must advertise a zero counter", reason, counter.Raw)
		}
	}
	patch(`{"history-mode":"serialized-context"}`, 200)
	if gjson.Get(status(), "enabled").Bool() || gjson.Get(status(), "history_mode").String() != "serialized-context" {
		t.Fatal("history setting implicitly enabled Web or failed to apply")
	}
	patch(`{"history-mode":"unknown"}`, 422)
	if gjson.Get(status(), "history_mode").String() != "serialized-context" {
		t.Fatal("rejected mode changed runtime")
	}
	patch(`{"enabled":true}`, 200)
	ctx, lease, err := h.authManager.WebRuntime().Acquire(context.Background(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	patch(`{"history-mode":"single-turn"}`, 200)
	if lease.HistoryMode() != "serialized-context" || gjson.Get(status(), "history_mode").String() != "single-turn" {
		t.Fatal("history snapshot or hot update failed")
	}
	h.authManager.WebRuntime().RecordPreflight(ctx, web.InputFailure(400, "invalid_history", "contents", "Invalid history"))
	if gjson.Get(status(), "preflight_rejections.invalid_history").Int() != 1 {
		t.Fatal("rejection status missing")
	}
	patch(`{"enabled":false}`, 200)
	saved := webManagementRequest(t, router, "GET", "/v8/management/config.yaml", nil, nil)
	parsed, err := config.ParseConfigBytes(saved.Body.Bytes())
	if err != nil || parsed.GeminiEnterpriseWeb.HistoryMode != "single-turn" || parsed.GeminiEnterpriseWeb.Enabled {
		t.Fatal("v8 YAML round trip lost Web settings", saved.Code, err)
	}
}
