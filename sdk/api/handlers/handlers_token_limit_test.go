package handlers

import "testing"

func TestParseModelTokenLimit(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		// Models with -Nm suffix → should extract limit
		{"claude-opus-4-6-thinking-20m", 200000},
		{"claude-opus-4-6-thinking-50m", 500000},
		{"claude-opus-4-6-thinking-100m", 1000000},
		{"claude-sonnet-4-6-20m", 200000},
		{"claude-sonnet-4-6-50m", 500000},
		{"claude-sonnet-4-6-100m", 1000000},
		{"any-model-10m", 100000},

		// Models without -Nm suffix → should return 0 (no limit)
		{"claude-opus-4-6-thinking", 0},
		{"claude-sonnet-4-6", 0},
		{"gemini-3-pro-preview", 0},
		{"gemini-2.5-flash", 0},
		{"gpt-5-codex", 0},
		{"", 0},
		{"m", 0},
		{"-m", 0},
		{"model-0m", 0},   // 0 is not valid
		{"model--5m", 50000}, // edge case: parses "5" after last "-", acceptable
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := parseModelTokenLimit(tt.model)
			if got != tt.want {
				t.Errorf("parseModelTokenLimit(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}
