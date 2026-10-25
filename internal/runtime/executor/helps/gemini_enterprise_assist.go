package helps

import (
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
