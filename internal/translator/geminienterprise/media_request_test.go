package geminienterprise

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestMediaRequestControlsAndFinalBarrier(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"assistant","content":"image","images":[{"image_url":{"url":"private"}}]},{"role":"user","content":"edit it"}]}`,
		`{"messages":[{"role":"user","content":"cat","videos":[]}]}`,
		`{"messages":[{"role":"user","content":"cat"}],"stream_options":{"include_usage":"yes"}}`,
	} {
		if _, err := NormalizeMedia([]byte(body), "openai", "alias", "image"); err == nil {
			t.Fatal("silently discarded reference media or controls")
		}
	}
	for _, tc := range []struct{ protocol, body, kind string }{
		{"openai", `{"messages":[{"role":"user","content":"a cat"}],"stream":true}`, "image"},
		{"gemini", `{"contents":[{"parts":[{"text":"a cat"}]}],"generationConfig":{"responseModalities":["TEXT","IMAGE"]}}`, "image"},
		{"openai-image", `{"model":"alias","prompt":"a cat","n":1,"response_format":"b64_json"}`, "image"},
		{"openai-video", `{"prompt":"a cat"}`, "video"},
		{"gemini-video", `{"instances":[{"prompt":"a cat"}],"parameters":{"sampleCount":1}}`, "video"},
	} {
		input, err := NormalizeMedia([]byte(tc.body), tc.protocol, "alias", tc.kind)
		if err != nil {
			t.Fatalf("%s: %v", tc.protocol, err)
		}
		if strings.Contains(string(input.Body), "modelId") || !strings.Contains(string(input.Body), tc.kind+"GenerationSpec") {
			t.Fatal("virtual model leaked upstream")
		}
		if err = ValidateMediaFinal(input.Body, tc.kind); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{`{"prompt":"cat","size":"1024x1024"}`, `{"prompt":"cat","n":2}`, `{"prompt":"cat","image":"private"}`, `{"prompt":"cat","response_format":"url"}`, `{"prompt":"cat","stream":true}`, `{"prompt":"cat","prompt":"dog"}`} {
		if _, err := NormalizeMedia([]byte(body), "openai-image", "alias", "image"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, body := range []string{`{"instances":[{"prompt":"cat","image":{}}]}`, `{"instances":[{"prompt":"cat"}],"parameters":{"durationSeconds":8}}`, `{"instances":[{"prompt":"cat"}],"parameters":{"sampleCount":2}}`} {
		if _, err := NormalizeMedia([]byte(body), "gemini-video", "alias", "video"); err == nil {
			t.Fatal("accepted unsupported native video controls")
		}
	}
	for _, body := range []string{`{"query":{"text":"cat"}}`, `{"query":{"text":"cat"},"toolsSpec":{"imageGenerationSpec":{}},"generationSpec":{"modelId":"alias"}}`, `{"query":{"text":"cat"},"toolsSpec":{"videoGenerationSpec":{}}}`} {
		if err := ValidateMediaFinal([]byte(body), "image"); err == nil {
			t.Fatal("repaired an invalid final payload")
		}
	}
}

func TestOfficialImageModelNativeGenerateContentBuildsImageToolRequest(t *testing.T) {
	for _, model := range []string{"gemini-3-pro-image", "portrait", "work/portrait"} {
		body := []byte(`{"contents":[{"role":"user","parts":[{"text":"A copper kettle on a white background"}]}],"generationConfig":{"responseModalities":["TEXT","IMAGE"],"candidateCount":1}}`)
		input, err := NormalizeMedia(body, "gemini", model, "image")
		if err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if input.MediaKind != "image" || !strings.Contains(gjson.GetBytes(input.Body, "query.text").String(), "A copper kettle on a white background") {
			t.Fatalf("native prompt or media capability lost: %s", input.Body)
		}
		if !gjson.GetBytes(input.Body, "toolsSpec.imageGenerationSpec").IsObject() || gjson.GetBytes(input.Body, "generationSpec").Exists() || gjson.GetBytes(input.Body, "model").Exists() {
			t.Fatalf("image model was sent as a text model or native controls leaked upstream: %s", input.Body)
		}
		if err := ValidateMediaFinal(input.Body, "image"); err != nil {
			t.Fatal(err)
		}
	}
}
