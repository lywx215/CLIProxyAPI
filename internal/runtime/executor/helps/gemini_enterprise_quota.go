package helps

import (
	"net/http"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func EnterpriseGenerationError(raw []byte, status int, header http.Header, auth *coreauth.Auth, started bool) error {
	err := ge.ParseGenerationError(raw, status, header, ge.FromMetadata(auth.Metadata), auth.EnterpriseQuotaIdentity, time.Now(), started)
	err.Diagnostic = enterprise.UpstreamHTTPError(err.HTTPStatus, raw)
	return err
}
