package enterpriseweb

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/core"
)

// RequireGoogleSearch reads the selected widget's current setting. Cached
// authentication and a web-grounding toggle cannot identify the search index.
func RequireGoogleSearch(ctx context.Context, base *http.Client, c Credential, token string) error {
	response, err := lookupWidgetConfig(ctx, base, c, token, time.Now())
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err = ReadJSON(response, &raw); err != nil {
		return err
	}
	mode, err := core.WidgetGroundingType(raw)
	if err != nil {
		return Fail(core.Status(err), "Could not verify the Plus application's Google Search setting")
	}
	if mode != "WEB_GROUNDING_TYPE_GOOGLE_SEARCH" {
		return &PreflightFailure{
			error:              Fail(http.StatusBadRequest, "The selected Gemini Enterprise Plus application must enable Google Search for web grounding"),
			SearchModeMismatch: true,
		}
	}
	return nil
}
