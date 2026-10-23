// Package geminienterprise implements the application-owned Google OAuth binding.
package geminienterprise

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	"golang.org/x/oauth2"
)

const Provider = "gemini-enterprise"
const Origin = "https://discoveryengine.googleapis.com"

func OAuth(c config.GeminiEnterpriseOAuth, redirect string) (*oauth2.Config, error) {
	c = c.WithDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	secret := ""
	if c.ClientSecretEnv != "" {
		secret = os.Getenv(c.ClientSecretEnv)
		if secret == "" {
			return nil, fmt.Errorf("gemini-enterprise: configured client secret environment variable is empty")
		}
	}
	return &oauth2.Config{ClientID: c.ClientID, ClientSecret: secret, RedirectURL: redirect,
		Scopes:   []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile"},
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}}, nil
}

// Client has no inference or discovery deadline. Credential acquisition callers may set one.
func Client(proxy string) (*http.Client, error) {
	transport, _, err := proxyutil.BuildHTTPTransport(proxy)
	if err != nil {
		return nil, fmt.Errorf("gemini-enterprise: invalid proxy configuration")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if transport != nil {
		client.Transport = transport
	}
	return client, nil
}

func Binding(c config.GeminiEnterpriseOAuth) string {
	c = c.WithDefaults()
	data, _ := json.Marshal(c)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func Engine(c config.GeminiEnterpriseOAuth) string {
	c = c.WithDefaults()
	return "projects/" + c.ProjectID + "/locations/" + c.Location + "/collections/" + c.CollectionID + "/engines/" + c.EngineID
}

func Metadata(c config.GeminiEnterpriseOAuth, email string, token *oauth2.Token) map[string]any {
	c = c.WithDefaults()
	m := map[string]any{"type": Provider, "email": email, "client_id": c.ClientID, "client_secret_env": c.ClientSecretEnv, "project_id": c.ProjectID, "location": c.Location, "collection_id": c.CollectionID, "engine_id": c.EngineID, "assistant_id": c.AssistantID}
	PutToken(m, token)
	return m
}

func PutToken(m map[string]any, token *oauth2.Token) {
	m["access_token"] = token.AccessToken
	if token.RefreshToken != "" {
		m["refresh_token"] = token.RefreshToken
	}
	m["token_type"] = token.TokenType
	m["expiry"] = token.Expiry.UTC().Format(time.RFC3339Nano)
	m["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
}

func String(m map[string]any, key string) string { value, _ := m[key].(string); return value }

func FromMetadata(m map[string]any) config.GeminiEnterpriseOAuth {
	return (config.GeminiEnterpriseOAuth{ClientID: String(m, "client_id"), ClientSecretEnv: String(m, "client_secret_env"), ProjectID: String(m, "project_id"), Location: String(m, "location"), CollectionID: String(m, "collection_id"), EngineID: String(m, "engine_id"), AssistantID: String(m, "assistant_id")}).WithDefaults()
}

func Official(req *http.Request) bool {
	return req != nil && req.URL != nil && req.URL.Scheme == "https" && req.URL.Host == "discoveryengine.googleapis.com" && req.URL.User == nil && req.URL.Opaque == ""
}

// TokenError deliberately omits token endpoint response bodies and client credentials.
func TokenError(err error) error {
	var retrieval *oauth2.RetrieveError
	if errors.As(err, &retrieval) && retrieval.ErrorCode == "invalid_grant" {
		return fmt.Errorf("gemini-enterprise: invalid_grant; Google authorization must be renewed")
	}
	return fmt.Errorf("gemini-enterprise: OAuth credential exchange failed")
}

func Refresh(ctx context.Context, c config.GeminiEnterpriseOAuth, refreshToken, proxy string) (*oauth2.Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("gemini-enterprise: no refresh token; Google authorization must be renewed")
	}
	oauth, err := OAuth(c, "")
	if err != nil {
		return nil, err
	}
	client, err := Client(proxy)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	token, err := oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, TokenError(err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("gemini-enterprise: empty OAuth access token")
	}
	return token, nil
}

type Model struct {
	ID          string
	DisplayName string
}

func Discover(ctx context.Context, client *http.Client, c config.GeminiEnterpriseOAuth, token string) ([]Model, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Origin+"/v1alpha/"+Engine(c)+"/widgetConfigs/default_search_widget_config?modelInfoView=END_USER_WEB&languageCode=en", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Goog-User-Project", c.ProjectID)
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := clientCopy.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini-enterprise: model discovery transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini-enterprise: model discovery returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 {
		return nil, fmt.Errorf("gemini-enterprise: invalid model discovery body")
	}
	return ParseModels(data)
}

func ParseModels(data []byte) ([]Model, error) {
	var body struct {
		UISettings struct {
			ModelConfigInfo struct {
				ResolvedModels *[]json.RawMessage `json:"resolvedModels"`
			} `json:"modelConfigInfo"`
		} `json:"uiSettings"`
	}
	if err := json.Unmarshal(data, &body); err != nil || !utf8.Valid(data) || body.UISettings.ModelConfigInfo.ResolvedModels == nil {
		return nil, fmt.Errorf("gemini-enterprise: missing or invalid resolvedModels catalog")
	}
	models := make([]Model, 0)
	seen := map[string]bool{}
	for _, raw := range *body.UISettings.ModelConfigInfo.ResolvedModels {
		var entry struct {
			ModelID     string `json:"modelId"`
			DisplayName string `json:"displayName"`
			ModelName   string `json:"modelName"`
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(raw, &entry) != nil {
			return nil, fmt.Errorf("gemini-enterprise: invalid resolvedModels entry")
		}
		id := strings.TrimSpace(entry.ModelID)
		if id == "" || seen[id] {
			continue
		}
		// Nonempty virtual IDs are advertised by the upstream too; never invent an Auto ID.
		if strings.ContainsAny(id, "\r\n\x00") {
			return nil, fmt.Errorf("gemini-enterprise: invalid model ID in catalog")
		}
		name := entry.DisplayName
		if name == "" {
			name = entry.ModelName
		}
		if name == "" {
			name = id
		}
		models = append(models, Model{ID: id, DisplayName: name})
		seen[id] = true
	}
	return models, nil
}

// Email uses only the fixed Google user-info origin and never follows redirects.
func Email(ctx context.Context, client *http.Client, token string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r, err := clientCopy.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini-enterprise: user info request failed")
	}
	defer func() { _ = r.Body.Close() }()
	var body struct {
		Email string `json:"email"`
	}
	if r.StatusCode != 200 || json.NewDecoder(io.LimitReader(r.Body, 1024*1024)).Decode(&body) != nil || body.Email == "" {
		return "", fmt.Errorf("gemini-enterprise: Google did not return an account email")
	}
	return body.Email, nil
}

// AuthorizationURL requests offline access with S256 PKCE and a random caller-owned state.
func AuthorizationURL(c *oauth2.Config, state, verifier string) string {
	return c.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "consent"))
}
