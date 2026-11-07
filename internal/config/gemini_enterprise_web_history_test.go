package config

import "testing"

func TestEnterpriseWebHistoryConfig(t *testing.T) {
	for _, mode := range []string{"", "single-turn", "serialized-context", "native", "FALSE"} {
		body := "config-version: 8\nupstream:\n  gemini-enterprise-web:\n    enabled: false\n"
		if mode != "" {
			body += "    history-mode: " + mode + "\n"
		}
		cfg, err := ParseConfigBytes([]byte(body))
		valid := mode == "" || mode == "single-turn" || mode == "serialized-context"
		if valid != (err == nil) {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if valid {
			want := mode
			if want == "" {
				want = EnterpriseWebSingleTurn
			}
			if cfg.GeminiEnterpriseWeb.WithDefaults().HistoryMode != want || cfg.GeminiEnterpriseWeb.Enabled {
				t.Fatal("mode changed channel state")
			}
			if err := ValidateV8Config([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
}
