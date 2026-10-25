package geminienterprise

import (
	"strings"
	"testing"
)

func TestEnterpriseMediaModelsRequireFeatureEvidence(t *testing.T) {
	for _, tc := range []struct {
		features, configs string
		want              int
	}{
		{`{}`, `{}`, 1},
		{`{}`, `{"gemini-3-pro-image":"MODEL_ENABLED"}`, 1},
		{`{"disable-image-generation":"FEATURE_STATE_ON"}`, `{"gemini-3-pro-image":"MODEL_ENABLED"}`, 1},
		{`{"disable-image-generation":"FEATURE_STATE_OFF"}`, `{}`, 1},
		{`{"disable-image-generation":"FEATURE_STATE_OFF"}`, `{"gemini-3-pro-image":"MODEL_DISABLED"}`, 1},
		{`{"disable-image-generation":"FEATURE_STATE_OFF"}`, `{"gemini-3-pro-image":"MODEL_ENABLED","gemini-2.5-flash-image":"MODEL_DISABLED","gemini-3.1-flash-image":"MODEL_DISABLED"}`, 2},
		{`{"disable-image-generation":"FEATURE_STATE_OFF","disable-video-generation":"FEATURE_STATE_OFF"}`, `{"gemini-3-pro-image":"MODEL_ENABLED"}`, 3},
	} {
		models, err := ParseModels([]byte(`{"uiSettings":{"features":` + tc.features + `,"modelConfigs":` + tc.configs + `,"modelConfigInfo":{"resolvedModels":[{"modelId":"text"}]}}}`))
		if err != nil || len(models) != tc.want {
			t.Fatalf("catalog=%v err=%v", models, err)
		}
		if tc.want >= 2 && (models[1].ID != "gemini-3-pro-image" || models[1].MediaKind != "image") {
			t.Fatal("incorrect application media capabilities")
		}
		if tc.want == 3 && (models[2].ID != "veo-3.1" || models[2].MediaKind != "video" || models[2].DisplayName != "Veo 3.1 (Enterprise default variant)") {
			t.Fatal("incorrect default video tool entry")
		}
		for _, model := range models {
			if strings.HasPrefix(model.ID, "gemini-enterprise-") {
				t.Fatal("published an invented media model ID")
			}
		}
	}
}

func TestEnterpriseVideoDiscoveryRequiresEnabledApplicationTool(t *testing.T) {
	for _, tc := range []struct {
		features string
		want     bool
	}{
		{`{}`, false},
		{`{"disable-video-generation":"FEATURE_STATE_ON"}`, false},
		{`{"disable-video-generation":"FEATURE_STATE_UNSPECIFIED"}`, false},
		{`{"disable-video-generation":"FEATURE_STATE_OFF"}`, true},
	} {
		for _, resolved := range []string{
			`[{"modelId":"text"}]`,
			`[{"modelId":"text"},{"modelId":"veo-3.1"},{"modelId":"veo-3.1"}]`,
		} {
			models, err := ParseModels([]byte(`{"uiSettings":{"features":` + tc.features + `,"modelConfigInfo":{"resolvedModels":` + resolved + `}}}`))
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.want {
				want++
			}
			if len(models) != want || models[0].ID != "text" {
				t.Fatalf("video feature state or deduplication ignored: %+v", models)
			}
			if tc.want && (models[1].ID != "veo-3.1" || models[1].MediaKind != "video" || models[1].DisplayName != "Veo 3.1 (Enterprise default variant)") {
				t.Fatalf("default video tool published with the wrong contract: %+v", models[1])
			}
		}
	}
}

func TestEnterpriseImageDiscoveryDeduplicatesOfficialModelAndPreservesCapability(t *testing.T) {
	models, err := ParseModels([]byte(`{"uiSettings":{"features":{"disable-image-generation":"FEATURE_STATE_OFF"},"modelConfigs":{"gemini-3-pro-image":"MODEL_ENABLED"},"modelConfigInfo":{"resolvedModels":[{"modelId":"text"},{"modelId":"gemini-3-pro-image","displayName":"Nano Banana Pro"},{"modelId":"gemini-3-pro-image"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[1].ID != "gemini-3-pro-image" || models[1].MediaKind != "image" {
		t.Fatalf("official image model lost its capability or was duplicated: %+v", models)
	}
}

func TestEnterpriseDisabledImageModelCannotLeakThroughTextCatalog(t *testing.T) {
	for _, tc := range []struct{ features, configs string }{
		{`{}`, `{"gemini-3-pro-image":"MODEL_ENABLED"}`},
		{`{"disable-image-generation":"FEATURE_STATE_ON"}`, `{"gemini-3-pro-image":"MODEL_ENABLED"}`},
		{`{"disable-image-generation":"FEATURE_STATE_OFF"}`, `{}`},
		{`{"disable-image-generation":"FEATURE_STATE_OFF"}`, `{"gemini-3-pro-image":"MODEL_DISABLED"}`},
	} {
		models, err := ParseModels([]byte(`{"uiSettings":{"features":` + tc.features + `,"modelConfigs":` + tc.configs + `,"modelConfigInfo":{"resolvedModels":[{"modelId":"text"},{"modelId":"gemini-3-pro-image"}]}}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 1 || models[0].ID != "text" {
			t.Fatalf("disabled media model leaked into text catalog: %+v", models)
		}
	}
}

func TestEnterpriseDisabledFlashImageModelsStayExcluded(t *testing.T) {
	models, err := ParseModels([]byte(`{"uiSettings":{"features":{"disable-image-generation":"FEATURE_STATE_OFF"},"modelConfigs":{"gemini-3-pro-image":"MODEL_ENABLED","gemini-2.5-flash-image":"MODEL_DISABLED","gemini-3.1-flash-image":"MODEL_DISABLED"},"modelConfigInfo":{"resolvedModels":[{"modelId":"gemini-2.5-flash-image"},{"modelId":"gemini-3.1-flash-image"},{"modelId":"text"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "text" || models[1].ID != "gemini-3-pro-image" {
		t.Fatalf("disabled Flash image models were published: %+v", models)
	}
}
