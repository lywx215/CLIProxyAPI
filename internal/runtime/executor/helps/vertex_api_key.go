package helps

import (
	"context"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func VertexProjectHost(route config.VertexProjectRoute) (string, error) {
	if errRoute := route.Validate(); errRoute != nil {
		return "", errRoute
	}
	switch route.Location {
	case "global":
		return "aiplatform.googleapis.com", nil
	case "us", "eu":
		return "aiplatform." + route.Location + ".rep.googleapis.com", nil
	default:
		return route.Location + "-aiplatform.googleapis.com", nil
	}
}

// VertexAPIKeyModelURL builds the Vertex Express or project-scoped publisher path.
func VertexAPIKeyModelURL(route config.VertexProjectRoute, baseURL, model, action string) (string, error) {
	if !route.Configured() {
		if baseURL == "" {
			baseURL = "https://aiplatform.googleapis.com"
		}
		return fmt.Sprintf("%s/v1/publishers/google/models/%s:%s", baseURL, model, action), nil
	}
	host, errHost := VertexProjectHost(route)
	if errHost != nil {
		return "", errHost
	}
	if !config.ValidVertexPublisherModel(model) {
		return "", fmt.Errorf("invalid Vertex publisher model ID")
	}
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:%s", host, route.ProjectID, route.Location, model, action), nil
}

func ApplyVertexAPIKeyHeaders(req *http.Request, auth *cliproxyauth.Auth, apiKey string, clientHeaders http.Header) {
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
	}
	if auth != nil {
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes, clientHeaders)
	}
	if auth.VertexProjectRoute().Configured() {
		req.Host = req.URL.Host
		req.Header.Del("Host")
		req.Header.Del("Authorization")
		req.Header.Set("x-goog-api-key", apiKey)
	}
}

func NewVertexAPIKeyHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) *http.Client {
	client := NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	if auth.VertexProjectRoute().Configured() {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client
}
