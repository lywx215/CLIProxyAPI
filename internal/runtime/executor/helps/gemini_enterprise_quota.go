package helps

import (
	"net/http"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func EnterpriseGenerationError(raw []byte, status int, header http.Header, auth *coreauth.Auth, started bool, model ...string) error {
	var binding config.GeminiEnterpriseOAuth
	var identity *ge.QuotaIdentity
	if auth != nil {
		binding, identity = ge.FromMetadata(auth.Metadata), auth.EnterpriseQuotaIdentity
	}
	err := ge.ParseGenerationError(raw, status, header, binding, identity, time.Now(), started)
	if len(model) > 0 {
		err.Model = model[0]
	}
	err.Diagnostic = enterprise.UpstreamHTTPError(err.HTTPStatus, raw)
	if status == 0 || status >= 200 && status < 300 {
		err.Diagnostic = enterprise.UpstreamStreamError(err.HTTPStatus, raw)
	}
	return err
}
