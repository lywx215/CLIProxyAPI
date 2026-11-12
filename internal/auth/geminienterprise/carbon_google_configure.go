package geminienterprise

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// ErrCarbonGoogleCredentialChanged stops writes after the caller's frozen local
// authorization or application no longer matches. It contains no secret data.
var ErrCarbonGoogleCredentialChanged = errors.New("Carbon Google credential changed")

type CarbonGoogleConfigureResult struct {
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type carbonGoogleConfiguredResource struct {
	value   map[string]any
	changed bool
}

// CarbonGoogleConfigureCache is scoped to one request. It deduplicates shared
// Engine/Assistant references without caching credentials or creating tasks.
type CarbonGoogleConfigureCache struct {
	resources map[string]carbonGoogleConfiguredResource
}

func carbonGoogleResult(status, reason string, code int) CarbonGoogleConfigureResult {
	return CarbonGoogleConfigureResult{Status: status, Reason: reason, HTTPStatus: code}
}

func carbonGoogleError(ctx context.Context, err error, code int) CarbonGoogleConfigureResult {
	if errors.Is(err, ErrCarbonGoogleCredentialChanged) {
		return carbonGoogleResult("changed", "credential_changed", 409)
	}
	if ctx.Err() != nil {
		return carbonGoogleResult("failed", "cancelled", 0)
	}
	switch code {
	case 401:
		return carbonGoogleResult("failed", "google_authorization_required", code)
	case 403:
		return carbonGoogleResult("failed", "google_permission_denied", code)
	case 404:
		return carbonGoogleResult("failed", "google_resource_not_found", code)
	case 409, 412:
		return carbonGoogleResult("changed", "google_configuration_changed", code)
	case 429:
		return carbonGoogleResult("failed", "google_rate_limited", code)
	}
	if code >= 500 || err != nil {
		return carbonGoogleResult("failed", "google_unavailable", code)
	}
	return carbonGoogleResult("failed", "google_request_rejected", code)
}

func carbonGoogleRequest(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token, method, name, mask string, body any, guard func(context.Context) error) (map[string]any, int, error) {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	endpoint := OriginForLocation(binding.Location) + "/v1alpha/" + name
	if mask != "" {
		endpoint += "?updateMask=" + url.QueryEscape(mask)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil || !OfficialBinding(request, binding.ProjectID, binding.Location) {
		return nil, 0, ErrCarbonGoogleCredentialChanged
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Goog-User-Project", binding.ProjectID)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	copyClient := *client
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if guard != nil {
		if err := guard(ctx); err != nil {
			return nil, 0, err
		}
	}
	response, err := copyClient.Do(request)
	if err != nil {
		return nil, 0, errors.New("Google resource transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, nil
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 {
		return nil, response.StatusCode, errors.New("Google resource response incomplete")
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil || value == nil || value["name"] != name {
		return nil, response.StatusCode, errors.New("Google resource response invalid")
	}
	return value, response.StatusCode, nil
}

func carbonGoogleAllowedModels(assistant map[string]any) ([]any, bool) {
	rawGeneration := assistant["generationConfig"]
	if rawGeneration == nil {
		return nil, true
	}
	generation, ok := rawGeneration.(map[string]any)
	if !ok {
		return nil, false
	}
	rawModels := generation["allowedModelIds"]
	if rawModels == nil {
		return nil, true
	}
	models, ok := rawModels.([]any)
	if !ok || len(models) > 10000 {
		return nil, false
	}
	for _, model := range models {
		id, valid := model.(string)
		if !valid || strings.TrimSpace(id) == "" || len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") {
			return nil, false
		}
	}
	return append([]any(nil), models...), true
}

func admissionGoogleHasModel(models []any, target string) bool {
	for _, model := range models {
		if model == target {
			return true
		}
	}
	return false
}

func admissionGoogleAllowedRangePreserved(before, after map[string]any, target string) bool {
	prior, validBefore := carbonGoogleAllowedModels(before)
	next, validAfter := carbonGoogleAllowedModels(after)
	if !validBefore || !validAfter || len(prior) == 0 || len(next) == 0 {
		return false
	}
	expected := map[string]bool{target: true}
	actual := map[string]bool{}
	for _, model := range prior {
		expected[model.(string)] = true
	}
	for _, model := range next {
		actual[model.(string)] = true
	}
	return CarbonFingerprintValue(expected) == CarbonFingerprintValue(actual)
}

func admissionGooglePreserved(value map[string]any, assistant bool, target string) string {
	data, _ := json.Marshal(value)
	var clone map[string]any
	_ = json.Unmarshal(data, &clone)
	delete(clone, "updateTime")
	if assistant {
		if generation, ok := clone["generationConfig"].(map[string]any); ok {
			delete(generation, "allowedModelIds")
		}
	} else {
		if models, ok := clone["modelConfigs"].(map[string]any); ok {
			delete(models, target)
			if len(models) == 0 {
				delete(clone, "modelConfigs")
			}
		} else if clone["modelConfigs"] == nil {
			delete(clone, "modelConfigs")
		}
	}
	return CarbonFingerprintValue(clone)
}

func admissionGoogleResourceEnabled(value map[string]any, assistant bool, target string) bool {
	if assistant {
		models, valid := carbonGoogleAllowedModels(value)
		return valid && admissionGoogleHasModel(models, target)
	}
	models, ok := value["modelConfigs"].(map[string]any)
	return ok && models[target] == "MODEL_ENABLED"
}

// ConfigureCarbonGoogle changes only the explicitly bound Engine/Assistant.
// An implicit Assistant model range is kept implicit: only the Engine override
// is configured, and the result requires a separate generation test. No model
// range is inferred from a catalog. Configuration is not model-call evidence.
func ConfigureAdmissionGoogle(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string, guard func(context.Context) error, cache *CarbonGoogleConfigureCache, target string) CarbonGoogleConfigureResult {
	if ctx == nil {
		ctx = context.Background()
	}
	binding = binding.WithDefaults()
	if client == nil || token == "" || binding.Validate() != nil || !SupportedLocation(binding.Location) {
		return carbonGoogleResult("changed", "credential_changed", 409)
	}
	if cache == nil {
		cache = &CarbonGoogleConfigureCache{}
	}
	if cache.resources == nil {
		cache.resources = map[string]carbonGoogleConfiguredResource{}
	}
	engineName := Engine(binding)
	assistantName := engineName + "/assistants/" + binding.AssistantID
	assistant, found := cache.resources[assistantName]
	if !found {
		value, code, err := carbonGoogleRequest(ctx, client, binding, token, http.MethodGet, assistantName, "", nil, guard)
		if err != nil || code != http.StatusOK {
			return carbonGoogleError(ctx, err, code)
		}
		assistant.value = value
	}
	allowedModels, valid := carbonGoogleAllowedModels(assistant.value)
	if !valid {
		return carbonGoogleResult("failed", "google_response_invalid", http.StatusOK)
	}
	implicitAssistant := len(allowedModels) == 0
	engine, engineFound := cache.resources[engineName]
	if !engineFound {
		value, code, err := carbonGoogleRequest(ctx, client, binding, token, http.MethodGet, engineName, "", nil, guard)
		if err != nil || code != http.StatusOK {
			return carbonGoogleError(ctx, err, code)
		}
		if raw, exists := value["modelConfigs"]; exists && raw != nil {
			models, valid := raw.(map[string]any)
			if !valid {
				return carbonGoogleResult("failed", "google_response_invalid", code)
			}
			for _, state := range models {
				if _, valid := state.(string); !valid {
					return carbonGoogleResult("failed", "google_response_invalid", code)
				}
			}
		}
		engine.value = value
	}
	for _, resource := range []struct {
		name      string
		assistant bool
		state     *carbonGoogleConfiguredResource
	}{{engineName, false, &engine}, {assistantName, true, &assistant}} {
		if admissionGoogleResourceEnabled(resource.state.value, resource.assistant, target) {
			cache.resources[resource.name] = *resource.state
			continue
		}
		latest, code, err := carbonGoogleRequest(ctx, client, binding, token, http.MethodGet, resource.name, "", nil, guard)
		if err != nil || code != http.StatusOK {
			return carbonGoogleError(ctx, err, code)
		}
		if CarbonFingerprintValue(latest) != CarbonFingerprintValue(resource.state.value) {
			return carbonGoogleResult("changed", "google_configuration_changed", 409)
		}
		if resource.assistant && implicitAssistant {
			// Do not convert Google's implicit range into an explicit allowlist.
			// The final read also detects an independent administrator changing it
			// while the Engine override was being configured.
			resource.state.value = latest
			cache.resources[resource.name] = *resource.state
			continue
		}
		mask := "modelConfigs"
		payload := map[string]any{"name": resource.name}
		if resource.assistant {
			models, valid := carbonGoogleAllowedModels(latest)
			if !valid {
				return carbonGoogleResult("failed", "google_response_invalid", http.StatusOK)
			}
			payload["generationConfig"] = map[string]any{"allowedModelIds": append(models, target)}
			mask = "generationConfig.allowedModelIds"
		} else {
			models := map[string]any{}
			if current, ok := latest["modelConfigs"].(map[string]any); ok {
				for key, value := range current {
					models[key] = value
				}
			}
			models[target] = "MODEL_ENABLED"
			payload["modelConfigs"] = models
		}
		_, patchCode, patchErr := carbonGoogleRequest(ctx, client, binding, token, http.MethodPatch, resource.name, mask, payload, guard)
		if errors.Is(patchErr, ErrCarbonGoogleCredentialChanged) {
			return carbonGoogleError(ctx, patchErr, patchCode)
		}
		if patchErr == nil && patchCode != http.StatusOK && patchCode < 500 && patchCode != 408 {
			return carbonGoogleError(ctx, nil, patchCode)
		}
		// Even a transport error may follow a committed PATCH. Read once, never
		// replay it. A canceled caller leaves the result unknown for its next run.
		if ctx.Err() != nil {
			return carbonGoogleResult("failed", "write_result_unknown", patchCode)
		}
		confirmed, readCode, readErr := carbonGoogleRequest(ctx, client, binding, token, http.MethodGet, resource.name, "", nil, guard)
		if errors.Is(readErr, ErrCarbonGoogleCredentialChanged) {
			return carbonGoogleError(ctx, readErr, readCode)
		}
		if readErr != nil || readCode != http.StatusOK {
			return carbonGoogleResult("failed", "write_result_unknown", readCode)
		}
		if !admissionGoogleResourceEnabled(confirmed, resource.assistant, target) {
			return carbonGoogleResult("failed", "write_not_confirmed", readCode)
		}
		if admissionGooglePreserved(latest, resource.assistant, target) != admissionGooglePreserved(confirmed, resource.assistant, target) {
			return carbonGoogleResult("changed", "google_configuration_changed", 409)
		}
		if resource.assistant && !admissionGoogleAllowedRangePreserved(latest, confirmed, target) {
			return carbonGoogleResult("changed", "google_configuration_changed", 409)
		}
		resource.state.value, resource.state.changed = confirmed, true
		cache.resources[resource.name] = *resource.state
	}
	if implicitAssistant {
		return carbonGoogleResult("verification_required", "assistant_defaults_preserved", http.StatusOK)
	}
	if engine.changed || assistant.changed {
		return carbonGoogleResult("configured", "", http.StatusOK)
	}
	return carbonGoogleResult("unchanged", "", http.StatusOK)
}

func ConfigureCarbonGoogle(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string, guard func(context.Context) error, cache *CarbonGoogleConfigureCache) CarbonGoogleConfigureResult {
	return ConfigureAdmissionGoogle(ctx, client, binding, token, guard, cache, CarbonModel)
}
func carbonGoogleHasModel(models []any) bool { return admissionGoogleHasModel(models, CarbonModel) }
func carbonGoogleAllowedRangePreserved(before, after map[string]any) bool {
	return admissionGoogleAllowedRangePreserved(before, after, CarbonModel)
}
func carbonGooglePreserved(value map[string]any, assistant bool) string {
	return admissionGooglePreserved(value, assistant, CarbonModel)
}
func carbonGoogleResourceEnabled(value map[string]any, assistant bool) bool {
	return admissionGoogleResourceEnabled(value, assistant, CarbonModel)
}
