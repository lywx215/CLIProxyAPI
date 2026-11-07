package config

import (
	"fmt"
	"testing"
)

func TestEnterpriseWebInputLimitsConfiguration(t *testing.T) {
	base := "config-version: 8\nupstream:\n  gemini-enterprise-web:\n    enabled: false\n    history-mode: serialized-context\n"
	cfg, err := ParseConfigBytes([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	limits := cfg.GeminiEnterpriseWeb.WithDefaults().InputLimits
	if limits.MaxContents != 50 || limits.MaxTextBytes != 4<<20 || limits.MaxAttachments != 12 {
		t.Fatalf("unexpected Web defaults: %+v", limits)
	}
	custom := base + "    input-limits:\n      max-contents: 70\n      max-text-bytes: 8388608\n      max-attachments: 20\n      max-tool-depth: 32\n"
	if err := ValidateV8Config([]byte(custom)); err != nil {
		t.Fatal(err)
	}
	cfg, err = ParseConfigBytes([]byte(custom))
	if err != nil {
		t.Fatal(err)
	}
	limits = cfg.GeminiEnterpriseWeb.WithDefaults().InputLimits
	if limits.MaxContents != 70 || limits.MaxTextBytes != 8<<20 || limits.MaxAttachments != 20 || limits.MaxToolDepth != 32 || limits.MaxRequestBytes != 68<<20 || cfg.GeminiEnterpriseWeb.Enabled {
		t.Fatalf("partial configuration lost defaults or enabled Web: %+v", cfg.GeminiEnterpriseWeb)
	}
	for _, invalid := range []string{
		"max-contents: -1", "max-text-bytes: -1", "max-attachments: 129",
		"max-request-bytes: 1073741825", "max-json-object-fields: 4097",
		"max-tool-depth: 257", "max-tool-nodes: 1000001", "max-tool-array-entries: 65537",
		"max-contents: 100\n      max-segments: 50",
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
