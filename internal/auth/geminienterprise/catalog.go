package geminienterprise

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type CatalogFallback struct {
	Mode   string   `json:"mode"`
	Models []string `json:"models"`
}

type CatalogState struct {
	Source        string         `json:"source"`
	Status        string         `json:"status"`
	CheckedAt     *time.Time     `json:"checked_at,omitempty"`
	LastSuccessAt *time.Time     `json:"last_success_at,omitempty"`
	LastError     *MetadataError `json:"last_error,omitempty"`
}

var fallbackModelID = regexp.MustCompile(`^(?:gemini|veo)-[a-z0-9][a-z0-9._-]*$`)

func CatalogFallbackSettings(metadata map[string]any) (CatalogFallback, error) {
	out := CatalogFallback{Mode: "default", Models: []string{}}
	if raw := metadata["catalog_fallback"]; raw != nil {
		data, err := json.Marshal(raw)
		if err != nil {
			return out, fmt.Errorf("catalog_fallback must be an object")
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&out) != nil {
			return out, fmt.Errorf("catalog_fallback must contain mode and an optional models array")
		}
	}
	if out.Mode != "default" && out.Mode != "custom" && out.Mode != "disabled" {
		return out, fmt.Errorf("catalog_fallback mode must be default, custom or disabled")
	}
	if len(out.Models) > 256 {
		return out, fmt.Errorf("catalog_fallback contains too many models")
	}
	seen := map[string]bool{}
	models := []string{}
	for _, id := range out.Models {
		id = strings.TrimSpace(id)
		if len(id) > 128 || !fallbackModelID.MatchString(id) || IsAutoModel(id) {
			return out, fmt.Errorf("catalog_fallback requires exact upstream Gemini or Veo model IDs")
		}
		if (strings.HasPrefix(id, "veo-") || strings.Contains(id, "image")) && id != "gemini-3-pro-image" && id != "veo-3.1" {
			return out, fmt.Errorf("catalog_fallback supports only gemini-3-pro-image and veo-3.1 as media models")
		}
		if !seen[id] {
			seen[id] = true
			models = append(models, id)
		}
	}
	out.Models = models
	return out, nil
}

func FallbackCatalog(setting CatalogFallback, subscription SubscriptionObservation) ([]Model, string) {
	if (subscription.Status != "ok" && subscription.Status != "stale") || subscription.FreeTrial == nil || setting.Mode == "disabled" {
		return nil, "none"
	}
	ids, source := setting.Models, "custom"
	if setting.Mode == "default" {
		source = "default"
		ids = []string{"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro"}
		if !*subscription.FreeTrial || subscription.Edition == "business_standard" {
			ids = []string{ArgonModel}
		}
	}
	models := make([]Model, 0, len(ids))
	for _, id := range ids {
		model := Model{ID: id, DisplayName: id}
		switch id {
		case "gemini-3-pro-image":
			model.MediaKind, model.DisplayName = "image", "Nano Banana Pro (Gemini 3 Pro Image)"
		case "veo-3.1":
			model.MediaKind, model.DisplayName = "video", "Veo 3.1 (Enterprise default variant)"
		}
		models = append(models, model)
	}
	return models, source
}
