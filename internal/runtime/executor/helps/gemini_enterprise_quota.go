package helps

import (
	"net/http"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func EnterpriseGenerationError(raw []byte, status int, header http.Header, auth *coreauth.Auth, started bool) error {
	return ge.ParseGenerationError(raw, status, header, ge.FromMetadata(auth.Metadata), auth.EnterpriseQuotaIdentity, time.Now(), started)
}
