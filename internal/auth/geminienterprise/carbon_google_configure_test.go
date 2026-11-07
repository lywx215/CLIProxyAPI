package geminienterprise

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type carbonGoogleTransport func(*http.Request) (*http.Response, error)

func (f carbonGoogleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type carbonGoogleMock struct {
	t          *testing.T
	values     map[string]map[string]any
	reads      map[string]int
	patches    map[string]int
	before     func(*http.Request)
	patchError func(string) (int, error)
}

func carbonGoogleFixture(t *testing.T) (*carbonGoogleMock, *http.Client, config.GeminiEnterpriseOAuth) {
	t.Helper()
	binding := (config.GeminiEnterpriseOAuth{ClientID: "synthetic", ProjectID: "123", EngineID: "app"}).WithDefaults()
	engine := Engine(binding)
	assistant := engine + "/assistants/" + binding.AssistantID
	mock := &carbonGoogleMock{t: t, values: map[string]map[string]any{
		engine:    {"name": engine, "displayName": "keep engine", "modelConfigs": map[string]any{ArgonModel: "MODEL_ENABLED", "disabled-model": "MODEL_DISABLED"}, "features": map[string]any{"search": "FEATURE_STATE_ON"}},
		assistant: {"name": assistant, "generationConfig": map[string]any{"allowedModelIds": []any{ArgonModel, "existing-extra-model"}, "defaultModelId": ArgonModel, "systemInstruction": map[string]any{"additionalSystemInstruction": "keep system"}}, "webGroundingType": "WEB_GROUNDING_TYPE_GOOGLE_SEARCH", "tools": []any{map[string]any{"keep": true}}},
	}, reads: map[string]int{}, patches: map[string]int{}}
	return mock, &http.Client{Transport: carbonGoogleTransport(mock.roundTrip)}, binding
}

func (m *carbonGoogleMock) roundTrip(request *http.Request) (*http.Response, error) {
	name := strings.TrimPrefix(request.URL.Path, "/v1alpha/")
	if !OfficialResource(request) || request.Header.Get("Authorization") != "Bearer synthetic-token" || request.Header.Get("X-Goog-User-Project") != "123" {
		m.t.Fatal("configuration escaped its official bound authorization")
	}
	if m.before != nil {
		m.before(request)
	}
	if request.Method == http.MethodPatch {
		m.patches[name]++
		var payload map[string]any
		if json.NewDecoder(request.Body).Decode(&payload) != nil || len(payload) != 2 || payload["name"] != name {
			m.t.Fatal("PATCH was not restricted to its named configuration field")
		}
		if strings.Contains(name, "/assistants/") {
			if request.URL.Query().Get("updateMask") != "generationConfig.allowedModelIds" {
				m.t.Fatal("assistant PATCH widened its field mask")
			}
			generation := payload["generationConfig"].(map[string]any)
			if len(generation) != 1 {
				m.t.Fatal("assistant PATCH overwrote other generation settings")
			}
			m.values[name]["generationConfig"].(map[string]any)["allowedModelIds"] = generation["allowedModelIds"]
		} else {
			if request.URL.Query().Get("updateMask") != "modelConfigs" {
				m.t.Fatal("engine PATCH widened its field mask")
			}
			m.values[name]["modelConfigs"] = payload["modelConfigs"]
		}
		if m.patchError != nil {
			if status, err := m.patchError(name); status != 0 || err != nil {
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("secret upstream detail"))}, err
			}
		}
	} else if request.Method == http.MethodGet {
		m.reads[name]++
	} else {
		m.t.Fatal("configuration invoked an unsupported method")
	}
	data, _ := json.Marshal(m.values[name])
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
}

func TestCarbonGoogleConfigurePreservesModelsDefaultsToolsAndIsIdempotent(t *testing.T) {
	mock, client, binding := carbonGoogleFixture(t)
	engine, assistant := Engine(binding), Engine(binding)+"/assistants/"+binding.AssistantID
	beforeEngine := carbonGooglePreserved(mock.values[engine], false)
	beforeAssistant := carbonGooglePreserved(mock.values[assistant], true)
	result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
	if result.Status != "configured" || mock.patches[engine] != 1 || mock.patches[assistant] != 1 || carbonGooglePreserved(mock.values[engine], false) != beforeEngine || carbonGooglePreserved(mock.values[assistant], true) != beforeAssistant {
		t.Fatalf("configuration did not preserve unrelated settings: %+v", result)
	}
	if !reflect.DeepEqual(mock.values[assistant]["generationConfig"].(map[string]any)["allowedModelIds"], []any{ArgonModel, "existing-extra-model", CarbonModel}) {
		t.Fatal("assistant allowed range was narrowed")
	}
	reads := mock.reads[engine] + mock.reads[assistant]
	result = ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
	if result.Status != "unchanged" || mock.patches[engine] != 1 || mock.patches[assistant] != 1 || mock.reads[engine]+mock.reads[assistant] != reads+2 {
		t.Fatalf("already configured operation was not read-only: %+v", result)
	}
}

func TestCarbonGoogleConfigureImplicitRangePreservesAssistantAndRequiresVerification(t *testing.T) {
	for _, scenario := range []string{"empty", "missing", "null-models", "no-generation", "null-generation"} {
		t.Run(scenario, func(t *testing.T) {
			mock, client, binding := carbonGoogleFixture(t)
			assistant := Engine(binding) + "/assistants/" + binding.AssistantID
			generation := mock.values[assistant]["generationConfig"].(map[string]any)
			switch scenario {
			case "empty":
				generation["allowedModelIds"] = []any{}
			case "missing":
				delete(generation, "allowedModelIds")
			case "null-models":
				generation["allowedModelIds"] = nil
			case "no-generation":
				delete(mock.values[assistant], "generationConfig")
			case "null-generation":
				mock.values[assistant]["generationConfig"] = nil
			}
			original, _ := json.Marshal(mock.values[assistant])
			beforeEngine := carbonGooglePreserved(mock.values[Engine(binding)], false)
			cache := &CarbonGoogleConfigureCache{}
			for range 2 {
				result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, cache)
				current, _ := json.Marshal(mock.values[assistant])
				if result.Status != "verification_required" || result.Reason != "assistant_defaults_preserved" || result.HTTPStatus != 200 || mock.patches[Engine(binding)] != 1 || mock.patches[assistant] != 0 || string(original) != string(current) {
					t.Fatalf("implicit Assistant range was changed or claimed verified: %+v", result)
				}
			}
			if !carbonGoogleResourceEnabled(mock.values[Engine(binding)], false) || carbonGooglePreserved(mock.values[Engine(binding)], false) != beforeEngine {
				t.Fatal("Engine override did not preserve its other model settings")
			}
			result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
			if result.Status != "verification_required" || mock.patches[Engine(binding)] != 1 || mock.patches[assistant] != 0 {
				t.Fatal("a new operation rewrote the Engine or implicit Assistant range")
			}
		})
	}
}

func TestCarbonGoogleConfigureMalformedAllowedRangeNeverWrites(t *testing.T) {
	for _, invalid := range []any{[]any{1}, []any{""}, "all", map[string]any{}} {
		mock, client, binding := carbonGoogleFixture(t)
		assistant := Engine(binding) + "/assistants/" + binding.AssistantID
		mock.values[assistant]["generationConfig"].(map[string]any)["allowedModelIds"] = invalid
		result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
		if result.Status != "failed" || result.Reason != "google_response_invalid" || len(mock.patches) != 0 || mock.reads[Engine(binding)] != 0 {
			t.Fatalf("malformed range reached a write: %+v", result)
		}
	}
	mock, client, binding := carbonGoogleFixture(t)
	mock.values[Engine(binding)+"/assistants/"+binding.AssistantID]["generationConfig"] = "invalid"
	result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
	if result.Reason != "google_response_invalid" || len(mock.patches) != 0 {
		t.Fatal("malformed generation config reached a write")
	}
}

func TestCarbonGoogleConfigureImplicitRangeDetectsConcurrentAssistantChange(t *testing.T) {
	for _, scenario := range []string{"allowlist", "tools", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			mock, client, binding := carbonGoogleFixture(t)
			engine, assistant := Engine(binding), Engine(binding)+"/assistants/"+binding.AssistantID
			mock.values[assistant]["generationConfig"].(map[string]any)["allowedModelIds"] = []any{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			mock.patchError = func(name string) (int, error) {
				if name != engine {
					t.Fatal("implicit Assistant was written")
				}
				switch scenario {
				case "allowlist":
					mock.values[assistant]["generationConfig"].(map[string]any)["allowedModelIds"] = []any{ArgonModel}
				case "tools":
					mock.values[assistant]["tools"] = []any{}
				case "cancel":
					cancel()
				}
				return 0, nil
			}
			result := ConfigureCarbonGoogle(ctx, client, binding, "synthetic-token", nil, nil)
			if scenario == "cancel" {
				if result.Reason != "write_result_unknown" {
					t.Fatal(result)
				}
			} else if result.Status != "changed" || result.Reason != "google_configuration_changed" {
				t.Fatal(result)
			}
			if mock.patches[engine] != 1 || mock.patches[assistant] != 0 {
				t.Fatal("concurrent change caused an implicit range write or Engine replay")
			}
		})
	}
}

func TestCarbonGoogleConfigureDeduplicatesSharedReferences(t *testing.T) {
	mock, client, binding := carbonGoogleFixture(t)
	cache := &CarbonGoogleConfigureCache{}
	for range 2 {
		if result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, cache); result.Status != "configured" {
			t.Fatal(result)
		}
	}
	engine := Engine(binding)
	assistant := engine + "/assistants/" + binding.AssistantID
	if mock.patches[engine] != 1 || mock.patches[assistant] != 1 || mock.reads[engine] != 3 || mock.reads[assistant] != 3 {
		t.Fatal("shared resources were executed more than once")
	}
}

func TestCarbonGoogleConfigurePartialFailureCanResumeWithoutRewritingEngine(t *testing.T) {
	mock, client, binding := carbonGoogleFixture(t)
	engine := Engine(binding)
	assistant := engine + "/assistants/" + binding.AssistantID
	mock.patchError = func(name string) (int, error) {
		if name == assistant {
			// Simulate a definitive rejection, which did not commit.
			mock.values[assistant]["generationConfig"].(map[string]any)["allowedModelIds"] = []any{ArgonModel, "existing-extra-model"}
			return http.StatusForbidden, nil
		}
		return 0, nil
	}
	result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
	if result.Status != "failed" || result.Reason != "google_permission_denied" || mock.patches[engine] != 1 {
		t.Fatal(result)
	}
	mock.patchError = nil
	result = ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
	if result.Status != "configured" || mock.patches[engine] != 1 || mock.patches[assistant] != 2 {
		t.Fatalf("retry rewrote completed work: %+v", result)
	}
}

func TestCarbonGoogleConfigureUnknownWriteOnlyReadsBackNeverReplays(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "not-committed"}[applied], func(t *testing.T) {
			mock, client, binding := carbonGoogleFixture(t)
			engine := Engine(binding)
			mock.patchError = func(name string) (int, error) {
				if name == engine {
					if !applied {
						delete(mock.values[engine]["modelConfigs"].(map[string]any), CarbonModel)
					}
					return 0, errors.New("synthetic uncertain transport failure")
				}
				return 0, nil
			}
			result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
			if mock.patches[engine] != 1 || mock.reads[engine] != 3 {
				t.Fatalf("unknown write was replayed or not checked: %+v", result)
			}
			if applied && result.Status != "configured" || !applied && result.Reason != "write_not_confirmed" {
				t.Fatal(result)
			}
		})
	}
}

func TestCarbonGoogleConfigureDetectsConcurrentRemoteChangesAndCancellation(t *testing.T) {
	for _, scenario := range []string{"remote-before-write", "remote-after-write", "assistant-range-shrunk", "local-before-write", "cancel-after-write"} {
		t.Run(scenario, func(t *testing.T) {
			mock, client, binding := carbonGoogleFixture(t)
			engine := Engine(binding)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			mock.before = func(request *http.Request) {
				if request.Method == "GET" && strings.TrimPrefix(request.URL.Path, "/v1alpha/") == engine && mock.reads[engine] == 1 && scenario == "remote-before-write" {
					mock.values[engine]["displayName"] = "external update"
				}
			}
			mock.patchError = func(name string) (int, error) {
				if name == engine && scenario == "remote-after-write" {
					mock.values[engine]["features"] = map[string]any{"search": "FEATURE_STATE_OFF"}
				}
				if name == engine && scenario == "cancel-after-write" {
					cancel()
					return 0, context.Canceled
				}
				if name != engine && scenario == "assistant-range-shrunk" {
					mock.values[name]["generationConfig"].(map[string]any)["allowedModelIds"] = []any{CarbonModel}
				}
				return 0, nil
			}
			checks := 0
			guard := func(context.Context) error {
				checks++
				if scenario == "local-before-write" && checks == 4 {
					return ErrCarbonGoogleCredentialChanged
				}
				return nil
			}
			result := ConfigureCarbonGoogle(ctx, client, binding, "synthetic-token", guard, nil)
			if scenario == "cancel-after-write" {
				if result.Reason != "write_result_unknown" || mock.reads[engine] != 2 || mock.patches[engine] != 1 {
					t.Fatal(result)
				}
			} else if result.Status != "changed" {
				t.Fatal(result)
			}
			if scenario == "remote-before-write" || scenario == "local-before-write" {
				if len(mock.patches) != 0 {
					t.Fatal("configuration wrote after its state changed")
				}
			}
		})
	}
}

func TestCarbonGoogleConfigureSafeReadErrorsNeverStartWrites(t *testing.T) {
	for _, code := range []int{401, 403, 404, 429, 500, 302} {
		mock, _, binding := carbonGoogleFixture(t)
		requests := 0
		client := &http.Client{Transport: carbonGoogleTransport(func(request *http.Request) (*http.Response, error) {
			requests++
			if request.Method != "GET" || !OfficialBinding(request, binding.ProjectID, binding.Location) {
				t.Fatal("failed read resulted in a write or redirect")
			}
			return &http.Response{StatusCode: code, Header: http.Header{"Location": {"https://untrusted.example.test"}}, Body: io.NopCloser(strings.NewReader("private Google authentication detail"))}, nil
		})}
		result := ConfigureCarbonGoogle(t.Context(), client, binding, "synthetic-token", nil, nil)
		data, _ := json.Marshal(result)
		if result.Status != "failed" || result.HTTPStatus != code || requests != 1 || len(mock.patches) != 0 || strings.Contains(string(data), "private") || strings.Contains(string(data), "synthetic-token") {
			t.Fatalf("failed Google read was not safely isolated: %+v", result)
		}
	}
}
