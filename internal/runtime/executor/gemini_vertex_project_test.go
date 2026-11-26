package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiVertexProjectRoutesAndFinalPayloadRules(t *testing.T) {
	const projectID = "vertex-test-project"
	const apiKey = "vertex-test-key"
	const model = "gemini-4-argon"
	const answer = `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`
	modelRules := []config.PayloadModelRule{{Name: model, Protocol: "gemini"}}
	exec := NewGeminiVertexExecutor(&config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{Models: modelRules, Params: map[string]any{
			"contents.0.parts.0.text":          "configured turn",
			"generationConfig.maxOutputTokens": 41,
		}}},
		Filter: []config.PayloadFilterRule{{Models: modelRules, Params: []string{"contents.0.role"}}},
	}})
	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"model":"client-model","contents":[{"role":"model","parts":[{"text":"previous turn"}]}],"generationConfig":{"maxOutputTokens":32}}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}
	for _, route := range []struct {
		name     string
		location string
		host     string
		baseURL  string
	}{
		{name: "global", location: "global", host: "aiplatform.googleapis.com"},
		{name: "us", location: "us", host: "aiplatform.us.rep.googleapis.com"},
		{name: "eu", location: "eu", host: "aiplatform.eu.rep.googleapis.com"},
		{name: "europe-west2", location: "europe-west2", host: "europe-west2-aiplatform.googleapis.com"},
		{name: "asia-southeast1", location: "asia-southeast1", host: "asia-southeast1-aiplatform.googleapis.com"},
		{name: "express", host: "aiplatform.googleapis.com"},
		{name: "compatible", host: "vertex-relay.example.test", baseURL: "https://vertex-relay.example.test/relay"},
	} {
		auth := &cliproxyauth.Auth{ID: "vertex-test-" + route.name, Provider: "vertex", Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey: apiKey,
			"base_url":                   route.baseURL,
		}}
		path := "/v1/publishers/google/models/" + model
		if route.baseURL != "" {
			path = "/relay" + path
		}
		if route.location != "" {
			auth.Attributes[cliproxyauth.AttributeVertexProjectID] = projectID
			auth.Attributes[cliproxyauth.AttributeVertexLocation] = route.location
			auth.Attributes["header:Host"] = "different-host.example.test"
			auth.Attributes["header:Authorization"] = "Bearer unavailable"
			auth.Attributes["header:X-Goog-Api-Key"] = "other-key"
			path = fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/google/models/%s", projectID, route.location, model)
		}
		for _, operation := range []struct {
			action      string
			query       string
			contentType string
			response    string
		}{
			{action: "generateContent", contentType: "application/json", response: answer},
			{action: "streamGenerateContent", query: "alt=sse", contentType: "text/event-stream", response: "data: " + answer + "\n\n"},
			{action: "countTokens", contentType: "application/json", response: `{"totalTokens":7}`},
		} {
			t.Run(route.name+"/"+operation.action, func(t *testing.T) {
				calls := 0
				transport := roundTripperFunc(func(upstream *http.Request) (*http.Response, error) {
					calls++
					if upstream.Method != http.MethodPost || upstream.URL.Scheme != "https" || upstream.URL.Host != route.host || upstream.Host != route.host || upstream.URL.Path != path+":"+operation.action || upstream.URL.RawQuery != operation.query {
						t.Errorf("upstream request = %s %s (Host: %s)", upstream.Method, upstream.URL, upstream.Host)
					}
					if upstream.Header.Get("X-Goog-Api-Key") != apiKey || upstream.Header.Get("Authorization") != "" {
						t.Error("request did not carry only the configured API key")
					}
					body, errRead := io.ReadAll(upstream.Body)
					if errRead != nil {
						return nil, errRead
					}
					modelField := gjson.GetBytes(body, "model")
					if route.location != "" && modelField.Exists() || route.location == "" && modelField.String() != model {
						t.Errorf("wrong model field for route: %s", body)
					}
					if gjson.GetBytes(body, "contents.0.role").Exists() || gjson.GetBytes(body, "contents.0.parts.0.text").String() != "configured turn" || gjson.GetBytes(body, "contents.1.parts.0.text").String() != "previous turn" || gjson.GetBytes(body, "generationConfig.maxOutputTokens").Int() != 41 {
						t.Errorf("payload rules did not follow built-in normalization: %s", body)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {operation.contentType}}, Body: io.NopCloser(strings.NewReader(operation.response))}, nil
				})
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
				switch operation.action {
				case "generateContent":
					response, errExecute := exec.Execute(ctx, auth, req, opts)
					if errExecute != nil || gjson.GetBytes(response.Payload, "candidates.0.content.parts.0.text").String() != "OK" {
						t.Fatalf("GenerateContent response = %s, err = %v", response.Payload, errExecute)
					}
				case "streamGenerateContent":
					stream, errStream := exec.ExecuteStream(ctx, auth, req, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					var payload strings.Builder
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Errorf("stream chunk: %v", chunk.Err)
						}
						payload.Write(chunk.Payload)
					}
					if got := payload.String(); !strings.Contains(got, `"text":"OK"`) || !strings.Contains(got, `"finishReason":"STOP"`) {
						t.Errorf("missing completed stream content: %s", got)
					}
				case "countTokens":
					response, errCount := exec.CountTokens(ctx, auth, req, opts)
					if errCount != nil || gjson.GetBytes(response.Payload, "totalTokens").Int() != 7 {
						t.Fatalf("CountTokens response = %s, err = %v", response.Payload, errCount)
					}
				}
				if calls != 1 {
					t.Errorf("upstream calls = %d, want 1", calls)
				}
			})
		}
	}
}

func TestGeminiVertexProjectAPIKeyDoesNotFollowRedirect(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "vertex", Attributes: map[string]string{
		cliproxyauth.AttributeAPIKey:          "vertex-test-key",
		cliproxyauth.AttributeVertexProjectID: "vertex-test-project",
		cliproxyauth.AttributeVertexLocation:  "us",
	}}
	transport := roundTripperFunc(func(upstream *http.Request) (*http.Response, error) {
		if upstream.URL.Host != "aiplatform.us.rep.googleapis.com" {
			t.Fatalf("followed unexpected redirect: %s", upstream.URL)
		}
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": {"https://other.example.test/redirect"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"unexpected redirect"}}`)),
		}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
	_, errExecute := NewGeminiVertexExecutor(&config.Config{}).Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gemini-4-argon",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
	if errExecute == nil || !strings.Contains(errExecute.Error(), "unexpected redirect") {
		t.Fatalf("Execute() error = %v, want the upstream redirect response", errExecute)
	}
}
