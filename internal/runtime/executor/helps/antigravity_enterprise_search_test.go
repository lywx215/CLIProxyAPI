package helps

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/tidwall/gjson"
)

func TestBusinessAICodeSearchMapsStandardAliasesAndPreservesOtherTools(t *testing.T) {
	for _, alias := range []string{"googleSearch", "google_search"} {
		t.Run(alias, func(t *testing.T) {
			body := []byte(`{"contents":[{"parts":[{"text":"search"}]}],"generationConfig":{"temperature":0},"tools":[{"` + alias + `":{}},{"functionDeclarations":[{"name":"read","parameters":{"type":"object"}}]},{"codeExecution":{}},{"urlContext":{}}]}`)
			got, err := NormalizeBusinessAICodeSearchTools(body)
			if err != nil {
				t.Fatal(err)
			}
			if !gjson.GetBytes(got, "tools.0.enterpriseWebSearch").IsObject() || gjson.GetBytes(got, "tools.0."+alias).Exists() {
				t.Fatalf("standard search was not mapped: %s", got)
			}
			for _, path := range []string{"contents", "generationConfig", "tools.1", "tools.2", "tools.3"} {
				if gjson.GetBytes(got, path).Raw != gjson.GetBytes(body, path).Raw {
					t.Fatalf("unrelated field %s changed: %s", path, got)
				}
			}
			if !BusinessAICodeSearchToolsValid(got) {
				t.Fatal("mapped payload failed final validation")
			}
		})
	}
}

func TestBusinessAICodeSearchNativeOptionsAndDuplicateDeclarations(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"both standard aliases", `{"tools":[{"googleSearch":{},"google_search":{}}]}`, `{"tools":[{"enterpriseWebSearch":{}}]}`},
		{"separate standard aliases", `{"tools":[{"googleSearch":{}},{"google_search":{}}]}`, `{"tools":[{"enterpriseWebSearch":{}}]}`},
		{"native restrictions win", `{"tools":[{"googleSearch":{}},{"enterpriseWebSearch":{"excludeDomains":["example.test"],"blockingConfidence":"BLOCK_HIGH_AND_ABOVE"}}]}`, `{"tools":[{"enterpriseWebSearch":{"excludeDomains":["example.test"],"blockingConfidence":"BLOCK_HIGH_AND_ABOVE"}}]}`},
		{"same object with native snake alias", `{"tools":[{"googleSearch":{},"enterprise_web_search":{"exclude_domains":["example.test"]},"functionDeclarations":[{"name":"read"}]}]}`, `{"tools":[{"enterprise_web_search":{"exclude_domains":["example.test"]},"functionDeclarations":[{"name":"read"}]}]}`},
		{"native aliases deduplicate", `{"tools":[{"enterprise_web_search":{}},{"enterpriseWebSearch":{}}]}`, `{"tools":[{"enterprise_web_search":{}}]}`},
		{"duplicate keeps declarations", `{"tools":[{"googleSearch":{}},{"enterpriseWebSearch":{},"functionDeclarations":[{"name":"read"}]}]}`, `{"tools":[{"enterpriseWebSearch":{}},{"functionDeclarations":[{"name":"read"}]}]}`},
		{"empty unrelated tool retained", `{"tools":[{}, {"googleSearch":{}}, {"google_search":{}}]}`, `{"tools":[{}, {"enterpriseWebSearch":{}}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeBusinessAICodeSearchTools([]byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			var gotValue, wantValue any
			if json.Unmarshal(got, &gotValue) != nil || json.Unmarshal([]byte(test.want), &wantValue) != nil || !reflect.DeepEqual(gotValue, wantValue) {
				t.Fatalf("got %s, want %s", got, test.want)
			}
			if !BusinessAICodeSearchToolsValid(got) {
				t.Fatal("normalized search declaration rejected")
			}
		})
	}
}

func TestBusinessAICodeSearchRejectsUnsupportedOrAmbiguousRequests(t *testing.T) {
	tests := []struct{ body, code string }{
		{`{"tools":[{"googleSearch":{"timeRangeFilter":{"startTime":"2026-01-01T00:00:00Z","endTime":"2026-01-02T00:00:00Z"}}}]}`, "unsupported_search_options"},
		{`{"tools":[{"google_search":{"time_range_filter":{}}},{"enterpriseWebSearch":{}}]}`, "unsupported_search_options"},
		{`{"tools":[{"googleSearch":{"unknown":false}}]}`, "unsupported_search_options"},
		{`{"tools":[{"enterpriseWebSearch":{},"enterprise_web_search":{"excludeDomains":["example.test"]}}]}`, "conflicting_search_options"},
		{`{"tools":[{"enterprise_web_search":{"exclude_domains":["a.test"]}},{"enterpriseWebSearch":{"excludeDomains":["b.test"]}}]}`, "conflicting_search_options"},
		{`{"tools":[{"googleSearch":null}]}`, "invalid_search_tools"},
		{`{"tools":[{"googleSearch":[]}]}`, "invalid_search_tools"},
		{`{"tools":[{"googleSearch":true}]}`, "invalid_search_tools"},
		{`{"tools":[{"enterpriseWebSearch":false}]}`, "invalid_search_tools"},
		{`{"tools":{}}`, "invalid_search_tools"},
		{`{"tools":[null]}`, "invalid_search_tools"},
		{`{"tools":[1]}`, "invalid_search_tools"},
		{`{"tools":[{"googleSearch":{},"googleSearch":{}}]}`, "invalid_search_tools"},
		{`{"tools":[{"enterpriseWebSearch":{"excludeDomains":[],"excludeDomains":["other.test"]}}]}`, "invalid_search_tools"},
		{`{"tools":[]} {}`, "invalid_search_tools"},
		{`[]`, "invalid_search_tools"},
	}
	for _, test := range tests {
		t.Run(test.body, func(t *testing.T) {
			_, err := NormalizeBusinessAICodeSearchTools([]byte(test.body))
			var failure *ae.Error
			if !errors.As(err, &failure) || failure.HTTP != 400 || failure.Code != test.code || !failure.IsRequestScoped() || failure.Attempted {
				t.Fatalf("expected unattempted request-scoped 400/%s, got %v", test.code, err)
			}
			if BusinessAICodeSearchToolsValid([]byte(test.body)) {
				t.Fatal("invalid search payload passed final validation")
			}
		})
	}
}

func TestBusinessAICodeSearchFinalValidationNeverRestoresFilteredTools(t *testing.T) {
	for _, body := range []string{
		`{"contents":[{"parts":[{"text":"hi"}]}]}`,
		`{"tools":[]}`,
		`{"tools":null}`,
		`{"tools":[{"enterpriseWebSearch":{"excludeDomains":["example.test"]}}]}`,
		`{"tools":[{"enterprise_web_search":{"exclude_domains":["example.test"]}}]}`,
		`{"tools":[{"functionDeclarations":[{"name":"read"}]},{"codeExecution":{}},{"urlContext":{}}]}`,
	} {
		input := []byte(body)
		before := bytes.Clone(input)
		got, err := NormalizeBusinessAICodeSearchTools(input)
		if err != nil || !bytes.Equal(got, before) || !BusinessAICodeSearchToolsValid(input) || !bytes.Equal(input, before) {
			t.Fatalf("native or filtered payload was changed/rejected: %s; %v", got, err)
		}
	}
	for _, body := range []string{
		`{"tools":[{"googleSearch":{}}]}`,
		`{"tools":[{"google_search":{}}]}`,
		`{"tools":[{"enterpriseWebSearch":{},"enterprise_web_search":{}}]}`,
	} {
		input := []byte(body)
		before := bytes.Clone(input)
		if BusinessAICodeSearchToolsValid(input) || !bytes.Equal(input, before) {
			t.Fatalf("payload rules reintroduced an alias or final validation mutated it: %s", input)
		}
	}
}
