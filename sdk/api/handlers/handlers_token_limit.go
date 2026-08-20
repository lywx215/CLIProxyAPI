package handlers

import (
	"strconv"
	"strings"
)

// parseModelTokenLimit extracts an input-token limit from a -Nm model suffix.
// N is measured in units of 10,000 tokens, so -20m represents 200,000 tokens.
func parseModelTokenLimit(model string) int {
	model = strings.TrimSpace(model)
	if len(model) < 3 || model[len(model)-1] != 'm' {
		return 0
	}
	dashIndex := strings.LastIndex(model[:len(model)-1], "-")
	if dashIndex < 0 {
		return 0
	}
	units, err := strconv.Atoi(model[dashIndex+1 : len(model)-1])
	if err != nil || units <= 0 {
		return 0
	}
	return units * 10000
}
