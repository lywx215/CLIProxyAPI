package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestEnterpriseWebInputLimitsConfiguration(t *testing.T) {
	base := "config-version: 8\nupstream:\n  gemini-enterprise-web:\n    enabled: false\n    history-mode: serialized-context\n"
	cfg, err := ParseConfigBytes([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	limits := cfg.GeminiEnterpriseWeb.WithDefaults().InputLimits
	if limits.MaxContents != 0 || limits.MaxSegments != 0 || limits.MaxTextBytes != 4<<20 || limits.MaxAttachments != 12 || limits.MaxAttachmentBytes != 50<<20 {
		t.Fatalf("unexpected Web defaults: %+v", limits)
	}
	custom := base + "    input-limits:\n      max-contents: 50\n      max-text-bytes: 8388608\n      max-attachments: 20\n      max-tool-depth: 32\n"
	if err := ValidateV8Config([]byte(custom)); err != nil {
		t.Fatal(err)
	}
	cfg, err = ParseConfigBytes([]byte(custom))
	if err != nil {
		t.Fatal(err)
	}
	limits = cfg.GeminiEnterpriseWeb.WithDefaults().InputLimits
	if limits.MaxContents != 0 || limits.MaxTextBytes != 8<<20 || limits.MaxAttachments != 20 || limits.MaxToolDepth != 32 || limits.MaxRequestBytes != 68<<20 || cfg.GeminiEnterpriseWeb.Enabled {
		t.Fatalf("partial configuration lost defaults or enabled Web: %+v", cfg.GeminiEnterpriseWeb)
	}
	t.Run("legacy count limits are ignored without rewriting YAML", func(t *testing.T) {
		data := []byte(base + "    input-limits:\n      max-contents: 200\n      max-segments: 1\n")
		if err := ValidateV8Config(data); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		for _, loader := range []struct {
			name string
			load func() (*Config, error)
		}{
			{"bytes", func() (*Config, error) { return ParseConfigBytes(data) }},
			{"file", func() (*Config, error) { return LoadConfig(path) }},
		} {
			t.Run(loader.name, func(t *testing.T) {
				cfg, err := loader.load()
				if err != nil {
					t.Fatal(err)
				}
				limits := cfg.GeminiEnterpriseWeb.InputLimits
				if limits.MaxContents != 0 || limits.MaxSegments != 0 {
					t.Fatalf("legacy count limits still effective: %+v", limits)
				}
				reported, err := json.Marshal(limits)
				if err != nil || bytes.Contains(reported, []byte("max-contents")) || bytes.Contains(reported, []byte("max-segments")) {
					t.Fatalf("effective limits include legacy counts: %s (%v)", reported, err)
				}
			})
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, data) {
			t.Fatal("loading config rewrote legacy settings", err)
		}
	})
	for _, invalid := range []string{
		"max-text-bytes: -1", "max-attachments: 129",
		"max-request-bytes: 1073741825", "max-json-object-fields: 4097",
		"max-tool-depth: 257", "max-tool-nodes: 1000001", "max-tool-array-entries: 65537",
	} {
		t.Run(invalid, func(t *testing.T) {
			body := []byte(fmt.Sprintf("%s    input-limits:\n      %s\n", base, invalid))
			if _, err := ParseConfigBytes(body); err == nil {
				t.Fatal("invalid limits accepted")
			}
			if err := ValidateV8Config(body); err == nil {
				t.Fatal("v8 accepted invalid limits")
			}
		})
	}
}
