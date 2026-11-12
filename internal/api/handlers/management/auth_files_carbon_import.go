package management

import (
	"encoding/json"
	"fmt"
	"strings"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

func carbonManagedKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
	for _, segment := range strings.FieldsFunc(key, func(r rune) bool { return r == '.' || r == '[' || r == ']' }) {
		if segment != key && carbonManagedKey(segment) {
			return true
		}
	}
	return key == ge.HydrogenEnabledKey || key == ge.HydrogenEvidenceKey || key == "hydrogen_test_id" || key == "hydrogen_admission" || key == "hydrogen_admission_supported" || strings.HasPrefix(key, "enterprise_hydrogen_") || key == ge.CarbonEnabledKey || key == ge.CarbonEvidenceKey || key == "carbon_test_id" || key == "carbon_admission" || key == "carbon_admission_supported" || strings.HasPrefix(key, "enterprise_carbon_")
}

func containsCarbonManagedFields(value any) bool {
	switch data := value.(type) {
	case map[string]json.RawMessage:
		for key, encoded := range data {
			if carbonManagedKey(key) {
				return true
			}
			var child any
			if json.Unmarshal(encoded, &child) == nil && containsCarbonManagedFields(child) {
				return true
			}
		}
	case map[string]any:
		for key, child := range data {
			if carbonManagedKey(key) || containsCarbonManagedFields(child) {
				return true
			}
		}
	case []any:
		for _, child := range data {
			if containsCarbonManagedFields(child) {
				return true
			}
		}
	}
	return false
}

// Imported credentials cannot grant their own successful test evidence or enable
// an application. Trusted Store reloads do not pass through this import boundary.
func sanitizeCarbonCredentialImport(data []byte) ([]byte, error) {
	var metadata map[string]any
	if json.Unmarshal(data, &metadata) != nil {
		return data, nil
	}
	provider, _ := metadata["type"].(string)
	if strings.ToLower(strings.TrimSpace(provider)) != ge.Provider {
		return data, nil
	}
	changed := false
	var sanitize func(any) error
	sanitize = func(value any) error {
		switch values := value.(type) {
		case map[string]any:
			for key, child := range values {
				if carbonManagedKey(key) {
					canonical := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
					if canonical == ge.CarbonEnabledKey || canonical == ge.HydrogenEnabledKey {
						if _, valid := child.(bool); !valid {
							return fmt.Errorf("%s must be a boolean", canonical)
						}
						values[key], changed = false, true
					} else {
						delete(values, key)
						changed = true
					}
					continue
				}
				if err := sanitize(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range values {
				if err := sanitize(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := sanitize(metadata); err != nil {
		return nil, err
	}
	if !changed {
		return data, nil
	}
	return json.Marshal(metadata)
}
