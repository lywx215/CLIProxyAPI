package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

type GeminiEnterpriseAuthenticator struct{}

func NewGeminiEnterpriseAuthenticator() Authenticator             { return &GeminiEnterpriseAuthenticator{} }
func (GeminiEnterpriseAuthenticator) Provider() string            { return ge.Provider }
func (GeminiEnterpriseAuthenticator) RefreshLead() *time.Duration { return new(5 * time.Minute) }

func (GeminiEnterpriseAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil || cfg.OAuth.Providers.GeminiEnterprise == nil {
		return nil, fmt.Errorf("configure oauth.providers.gemini-enterprise before login")
	}
	if cfg.Home.Enabled {
		return nil, fmt.Errorf("gemini-enterprise OAuth is not supported in CLIProxyAPIHome mode")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	binding := cfg.OAuth.Providers.GeminiEnterprise.WithDefaults()
	port := 8788
	if opts.CallbackPort > 0 {
		port = opts.CallbackPort
	}
	if errValidate := binding.ValidateLogin(); errValidate != nil {
		return nil, errValidate
	}
	oauth, err := ge.ApplicationOAuth(binding, fmt.Sprintf("http://localhost:%d/oauth-callback", port))
	if err != nil {
		return nil, err
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		return nil, fmt.Errorf("gemini-enterprise: cannot generate OAuth state")
	}
	verifier := oauth2.GenerateVerifier()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("gemini-enterprise: cannot listen on callback port %d", port)
	}
	codes := make(chan enterpriseCallbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", enterpriseCallbackHandler(state, codes))
	server := &http.Server{Handler: mux}
	defer func() { _ = server.Close() }()
	go func() { _ = server.Serve(listener) }()
	authURL := ge.AuthorizationURL(oauth, state, verifier)
	fmt.Printf("Complete Google authorization in your browser:\n%s\n", authURL)
	if !opts.NoBrowser {
		_ = browser.OpenURL(authURL)
	}
	wait := time.NewTimer(5 * time.Minute)
	defer wait.Stop()
	var code string
	select {
	case result := <-codes:
		if result.denied {
			return nil, fmt.Errorf("gemini-enterprise: Google authorization was denied")
		}
		code = result.code
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-wait.C:
		return nil, fmt.Errorf("gemini-enterprise: OAuth callback timed out")
	}
	client, err := ge.Client(cfg.ProxyURL)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	credentialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	credentialCtx = context.WithValue(credentialCtx, oauth2.HTTPClient, client)
	token, err := oauth.Exchange(credentialCtx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, ge.TokenError(err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("gemini-enterprise: empty access token")
	}
	email, err := ge.Email(credentialCtx, client, token.AccessToken)
	if err != nil {
		return nil, err
	}
	// Only the verified Google principal and OAuth client own refresh credentials.
	var existing []*coreauth.Auth
	if store := GetTokenStore(); store != nil {
		if setter, ok := store.(interface{ SetBaseDir(string) }); ok {
			setter.SetBaseDir(cfg.AuthDir)
		}
		if records, errList := store.List(ctx); errList == nil {
			existing = records
		}
	}
	record, err := NewGeminiEnterpriseAccountRecord(binding, email, token, existing)
	if err != nil {
		return nil, err
	}
	if ge.String(record.Metadata, "refresh_token") != "" {
		fmt.Println("Gemini Enterprise automatic renewal is ready.")
	} else {
		fmt.Println("Google returned no refresh token; automatic renewal is unavailable. Authorize again to obtain offline access.")
	}
	return record, nil
}

// PostLogin runs after the manager has saved authorization. It updates the graph
// in memory; PostLoginNeedsPersistence tells the manager to save it with its store.
func (GeminiEnterpriseAuthenticator) PostLogin(ctx context.Context, cfg *config.Config, record *coreauth.Auth) {
	if cfg == nil || record == nil || record.Provider != ge.Provider || cfg.Home.Enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fmt.Println("OAuth saved; discovering projects and collections...")
	result, err := DiscoverGeminiEnterpriseAccount(ctx, cfg, record)
	if err != nil {
		fmt.Printf("Login succeeded; resource discovery failed: %v\n", err)
		return
	}
	fmt.Printf("Discovered %d collection bindings across %d projects (%s).\n", len(result.Collections), len(result.Projects), result.Status)
}

// PostLoginNeedsPersistence applies only to this authenticator's graph update.
func (GeminiEnterpriseAuthenticator) PostLoginNeedsPersistence() bool { return true }

// DiscoverGeminiEnterpriseAccount updates the supplied record only after callers
// have saved OAuth. Management callers should merge results into current state
// through the runtime manager instead when concurrent edits are possible.
func DiscoverGeminiEnterpriseAccount(ctx context.Context, cfg *config.Config, record *coreauth.Auth) (ge.DiscoveryResult, error) {
	if cfg == nil || record == nil || record.Provider != ge.Provider {
		return ge.DiscoveryResult{}, fmt.Errorf("gemini-enterprise: account record and configuration are required")
	}
	account, err := ge.ReadAccount(record.Metadata)
	if err != nil {
		return ge.DiscoveryResult{}, err
	}
	if account == nil {
		return ge.DiscoveryResult{}, fmt.Errorf("gemini-enterprise: an account credential is required")
	}
	proxy := record.ProxyURL
	if proxy == "" {
		proxy = ge.String(record.Metadata, "proxy_url")
	}
	if proxy == "" {
		proxy = cfg.ProxyURL
	}
	client, err := ge.Client(proxy)
	if err != nil {
		return ge.DiscoveryResult{}, err
	}
	defer client.CloseIdleConnections()
	result, errDiscover := ge.DiscoverResources(ctx, client, ge.FromMetadata(record.Metadata), ge.String(record.Metadata, "access_token"), account.Collections)
	merged, _ := ge.MergeDiscoveredCollections(*account, result)
	record.Metadata = ge.PutAccount(record.Metadata, merged)
	return result, errDiscover
}

func enterpriseRecordID(email string, binding config.GeminiEnterpriseOAuth) string {
	identity := sha256.Sum256([]byte(strings.ToLower(email) + "/" + ge.Binding(binding)))
	return fmt.Sprintf("gemini-enterprise-%x.json", identity[:16])
}
func retainedEnterpriseRefresh(records []*coreauth.Auth, email string, binding config.GeminiEnterpriseOAuth) string {
	for _, old := range records {
		if old != nil && old.Provider == ge.Provider && strings.EqualFold(ge.String(old.Metadata, "email"), email) && ge.Binding(ge.FromMetadata(old.Metadata)) == ge.Binding(binding) {
			if token := ge.String(old.Metadata, "refresh_token"); token != "" {
				return token
			}
		}
	}
	return ""
}

type enterpriseCallbackResult struct {
	code   string
	denied bool
}

func enterpriseCallbackHandler(state string, codes chan<- enterpriseCallbackResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("error") != "" {
			select {
			case codes <- enterpriseCallbackResult{denied: true}:
			default:
			}
			http.Error(w, "Google authorization was denied", http.StatusForbidden)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Google authorization was not completed", http.StatusBadRequest)
			return
		}
		select {
		case codes <- enterpriseCallbackResult{code: code}:
		default:
		}
		_, _ = w.Write([]byte("Authorization received. You may close this tab."))
	}
}
