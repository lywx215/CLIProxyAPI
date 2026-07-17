package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestBuildGeminiConfigModelsUsesClientVisibleAliasAsDisplayName(t *testing.T) {
	entry := &internalconfig.GeminiKey{
		Models: []internalconfig.GeminiModel{
			{
				Name:  "gemini-3.5-flash-medium",
				Alias: "gemini-3.1-pro-preview",
			},
		},
	}

	models := buildGeminiConfigModels(entry)
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].ID != "gemini-3.1-pro-preview" {
		t.Fatalf("model ID = %q, want %q", models[0].ID, "gemini-3.1-pro-preview")
	}
	if models[0].DisplayName != "gemini-3.1-pro-preview" {
		t.Fatalf("display name = %q, want client-visible alias %q", models[0].DisplayName, "gemini-3.1-pro-preview")
	}
}

func TestBuildGeminiConfigModelsUsesNameWhenAliasIsEmpty(t *testing.T) {
	entry := &internalconfig.GeminiKey{
		Models: []internalconfig.GeminiModel{
			{Name: "gemini-2.5-pro"},
		},
	}

	models := buildGeminiConfigModels(entry)
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].ID != "gemini-2.5-pro" || models[0].DisplayName != "gemini-2.5-pro" {
		t.Fatalf("model ID/display name = %q/%q, want %q/%q", models[0].ID, models[0].DisplayName, "gemini-2.5-pro", "gemini-2.5-pro")
	}
}
