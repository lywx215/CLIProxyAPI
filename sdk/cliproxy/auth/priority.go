package auth

import (
	"strconv"
	"strings"
)

// AttributeFilePriority marks priority values inherited from a physical auth file.
const AttributeFilePriority = "file_priority"

// CredentialRoutingValues reports the effective values used by the selectors.
func CredentialRoutingValues(auth *Auth) (priority int, weight int64) {
	return authPriority(auth), authWeight(auth)
}

// ApplyAuthPriorityMetadata copies a valid file priority into an auth's metadata and routing attributes.
func ApplyAuthPriorityMetadata(auth *Auth, metadata map[string]any) {
	if auth == nil {
		return
	}
	delete(auth.Attributes, AttributeFilePriority)
	rawPriority, ok := metadata["priority"]
	if !ok {
		return
	}
	priority := ""
	switch value := rawPriority.(type) {
	case float64:
		priority = strconv.Itoa(int(value))
	case string:
		priority = strings.TrimSpace(value)
		if _, errAtoi := strconv.Atoi(priority); errAtoi != nil {
			return
		}
	default:
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["priority"] = rawPriority
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["priority"] = priority
	auth.Attributes[AttributeFilePriority] = "true"
}
