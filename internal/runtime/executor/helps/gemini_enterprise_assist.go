package helps

import (
	"errors"
	"net/http"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyEnterpriseAssistDefaults runs before user payload rules, which may override
// or remove this preference. REQUEST_ASSIST retains Google's policy checks.
func ApplyEnterpriseAssistDefaults(body []byte) ([]byte, error) {
	if gjson.GetBytes(body, "assistSkippingMode").Exists() {
		return body, nil
	}
	updated, err := sjson.SetBytes(body, "assistSkippingMode", "REQUEST_ASSIST")
	if err != nil {
		return nil, enterprise.Invalid("cannot prepare Enterprise assistant generation")
	}
	return updated, nil
}

// EnterpriseAssistCompatibilityPayload recognizes an explicit Google schema
// rejection. The returned body is the only permissible payload for one retry.
func EnterpriseAssistCompatibilityPayload(body []byte, err error) []byte {
	if !gjson.GetBytes(body, "assistSkippingMode").Exists() {
		return nil
	}
	var rejected *ge.GenerationError
	if !errors.As(err, &rejected) || rejected.Started || rejected.UpstreamHTTPStatus != http.StatusBadRequest {
		return nil
	}
	diagnostic := enterprise.LogUpstreamDiagnostic(err, http.StatusBadRequest)
	if diagnostic == nil || diagnostic.Status != "INVALID_ARGUMENT" || diagnostic.Category != "unsupported_field" ||
		len(diagnostic.Fields) != 1 || diagnostic.Fields[0] != (enterprise.FieldViolation{Field: "assistSkippingMode", Reason: "unsupported_field"}) {
		return nil
	}
	updated, err := sjson.DeleteBytes(body, "assistSkippingMode")
	if err != nil {
		return nil
	}
	return updated
}
