package config

import (
	"strings"
	"testing"
)

func TestVertexProjectRoutesRoundTrip(t *testing.T) {
	const input = `vertex-api-key:
  - api-key: synthetic-key
    project-id: '123456789012'
    location: US
    models: [{name: gemini-4-carbon, alias: gemini-4-carbon-us}]
  - api-key: synthetic-key
    project-id: '123456789012'
    location: global
    models: []
  - api-key: synthetic-key
    project-id: other-project
    location: us
    models: [{name: gemini-4-carbon, alias: gemini-4-carbon-us}]
  - api-key: synthetic-key
`
	layout, changed, errLayout := NormalizeConfigLayout([]byte(input), true)
	if errLayout != nil || !changed {
		t.Fatalf("normalize Vertex groups: changed=%v, err=%v", changed, errLayout)
	}
	if err := ValidateV8Config(layout); err != nil {
		t.Fatal(err)
	}
	cfg, errParse := ParseConfigBytes(layout)
	if errParse != nil {
		t.Fatal(errParse)
	}
	keys := cfg.VertexCompatAPIKey
	if len(keys) != 4 || keys[0].ProjectRoute() != (VertexProjectRoute{ProjectID: "123456789012", Location: "us"}) || len(keys[1].Models) != 0 || keys[2].ProjectID != "other-project" || keys[3].ProjectRoute().Configured() {
		t.Fatal("project routes did not survive grouped config parsing")
	}
}

func TestVertexProjectRouteValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry string
		want  string
	}{
		{"partial scope", "project-id: test-project", "must be set together"},
		{"invalid project", "project-id: foo/bar, location: us", "Google Cloud project ID"},
		{"invalid location", "project-id: test-project, location: 'us/../eu'", "Google Cloud location"},
		{"custom host", "project-id: test-project, location: us, base-url: https://example.invalid", "cannot use base-url"},
		{"native interactions", "project-id: test-project, location: us, interactions: true", "interactions=true"},
		{"model resource path", "project-id: test-project, location: us, models: [{name: ../bad, alias: test}]", "publisher model ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "vertex-api-key: [{api-key: synthetic-key, " + tc.entry + "}]"
			_, err := ParseConfigBytes([]byte(input))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q error, got %v", tc.want, err)
			}
		})
	}
	const conflicting = `vertex-api-key:
  - {api-key: synthetic-key, project-id: test-project, location: us, models: [{name: gemini-4-carbon, alias: shared}]}
  - {api-key: synthetic-key, project-id: test-project, location: eu, models: [{name: gemini-4-carbon, alias: shared}]}`
	if _, err := ParseConfigBytes([]byte(conflicting)); err == nil || !strings.Contains(err.Error(), "already bound to us") {
		t.Fatalf("expected a conflict between two regions using the same alias, got %v", err)
	}
	duplicate := strings.Replace(conflicting, "location: eu", "location: us", 1)
	if _, err := ParseConfigBytes([]byte(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate project and location") {
		t.Fatalf("expected duplicate project and location to fail, got %v", err)
	}
}
