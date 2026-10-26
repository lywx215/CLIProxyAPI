package geminienterprise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/oauth2"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnterpriseOwnOAuthPKCEAndSecretIsolation(t *testing.T) {
	t.Setenv("ENTERPRISE_TEST_SECRET", "test-secret-value")
	binding := config.GeminiEnterpriseOAuth{ClientID: "own-client", ClientSecretEnv: "ENTERPRISE_TEST_SECRET", ProjectID: "project", EngineID: "engine"}
	oauth, err := OAuth(binding, "http://localhost:8788/oauth-callback")
	if err != nil {
		t.Fatal(err)
	}
	verifier := oauth2.GenerateVerifier()
	authURL, _ := url.Parse(AuthorizationURL(oauth, "random-state", verifier))
	q := authURL.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "offline" || q.Get("state") != "random-state" || q.Get("client_id") != "own-client" || q.Get("redirect_uri") != "http://localhost:8788/oauth-callback" {
		t.Fatal("incorrect OAuth authorization parameters")
	}
	if strings.Contains(authURL.String(), "test-secret-value") {
		t.Fatal("secret leaked into URL")
	}
	metadata := Metadata(binding, "account@example.com", &oauth2.Token{AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)})
	data, _ := json.Marshal(metadata)
	if strings.Contains(string(data), "test-secret-value") || Binding(FromMetadata(metadata)) != Binding(binding) {
		t.Fatal("secret or binding roundtrip violation")
	}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://oauth2.googleapis.com/token" {
			t.Fatalf("unexpected OAuth endpoint %s", req.URL)
		}
		_ = req.ParseForm()
		if req.Form.Get("client_id") != "own-client" || req.Form.Get("client_secret") != "test-secret-value" || req.Form.Get("refresh_token") != "refresh" {
			t.Fatal("refresh used another client or credential")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"new","expires_in":3600,"refresh_token":"rotated","token_type":"Bearer"}`))}, nil
	})
	token, err := Refresh(context.Background(), binding, "refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	PutToken(metadata, token)
	if String(metadata, "refresh_token") != "rotated" || String(metadata, "access_token") != "new" {
		t.Fatal("token rotation lost")
	}
	http.DefaultTransport = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"test-secret-value access refresh"}`))}, nil
	})
	_, err = Refresh(context.Background(), binding, "refresh", "")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "test-secret-value") {
		t.Fatal("OAuth error omitted reauthorization or leaked response")
	}
	t.Setenv("ENTERPRISE_TEST_SECRET", "")
	if _, err = OAuth(binding, ""); err == nil {
		t.Fatal("missing secret accepted")
	}
}

func TestEnterpriseRealCatalogParsing(t *testing.T) {
	models, err := ParseModels([]byte(`{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"gemini-4-argon","displayName":"Argon"},{"modelId":"","displayName":"Auto"},{"modelId":"new-virtual"},{"modelId":"gemini-4-argon"}]}}}`))
	if err != nil || len(models) != 2 || models[1].ID != "new-virtual" {
		t.Fatalf("models=%v err=%v", models, err)
	}
	models, err = ParseModels([]byte(`{"uiSettings":{"modelConfigInfo":{"resolvedModels":[]}}}`))
	if err != nil || len(models) != 0 {
		t.Fatal("successful empty catalog rejected")
	}
	for _, bad := range []string{`{}`, `null`, `{"uiSettings":{"modelConfigInfo":{"resolvedModels":null}}}`, `{"uiSettings":{"modelConfigInfo":{"resolvedModels":[null]}}}`, `{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":7}]}}}`, "{\"uiSettings\":{\"modelConfigInfo\":{\"resolvedModels\":[{\"modelId\":\"bad\xff\"}]}}}"} {
		if _, err := ParseModels([]byte(bad)); err == nil {
			t.Fatalf("invalid catalog accepted %s", bad)
		}
	}
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if _, deadline := req.Context().Deadline(); deadline {
			t.Error("discovery has an inference deadline")
		}
		if req.URL.Host != "discoveryengine.googleapis.com" || !strings.HasSuffix(req.URL.Path, "/widgetConfigs/default_search_widget_config") || req.URL.Query().Get("modelInfoView") != "END_USER_WEB" {
			t.Error("wrong discovery endpoint")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"uiSettings":{"modelConfigInfo":{"resolvedModels":[]}}}`))}, nil
	})}
	_, err = Discover(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "project", EngineID: "engine"}, "access")
	if err != nil {
		t.Fatal(err)
	}
}

func TestEnterpriseCredentialHelpersNeverFollowRedirects(t *testing.T) {
	if Official(&http.Request{URL: &url.URL{Scheme: "https", Host: "discoveryengine.googleapis.com", Opaque: "unexpected"}}) {
		t.Fatal("opaque URL was accepted as the fixed official origin")
	}
	for _, operation := range []string{"discover", "email"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://other.discoveryengine.googleapis.com/redirect"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			var err error
			if operation == "discover" {
				_, err = Discover(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "project", EngineID: "engine"}, "access")
			} else {
				_, err = Email(context.Background(), client, "access")
			}
			if err == nil || calls != 1 {
				t.Fatalf("credential request followed a redirect: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestEnterpriseRefreshPreservesCancellationAndSanitizesWrappedErrors(t *testing.T) {
	binding := config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "project", EngineID: "engine"}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTrip(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Refresh(ctx, binding, "refresh", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh cancellation lost: %v", err)
	}
	err = TokenError(fmt.Errorf("wrapped: %w", &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: "private credentials"}))
	if !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "private credentials") {
		t.Fatal("wrapped OAuth error leaked detail or lost invalid_grant")
	}
}

func TestEnterpriseApplicationOnlyRefreshAndVerifiedEmail(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://oauth2.googleapis.com/token" {
			t.Fatal("refresh left fixed OAuth origin")
		}
		return resourceResponse(200, `{"access_token":"synthetic-new","expires_in":3600,"token_type":"Bearer"}`), nil
	})
	if token, err := Refresh(context.Background(), config.GeminiEnterpriseOAuth{ClientID: "client"}, "synthetic-refresh", ""); err != nil || token.AccessToken != "synthetic-new" {
		t.Fatalf("application-only token refresh failed: %v", err)
	}
	for _, test := range []struct {
		body  string
		valid bool
	}{{`{"email":" Fixture@Example.com ","verified_email":true}`, true}, {`{"email":"fixture@example.com","verified_email":false}`, false}, {`{"email":"fixture@example.com"}`, false}} {
		client := &http.Client{Timeout: time.Nanosecond, Transport: roundTrip(func(req *http.Request) (*http.Response, error) { return resourceResponse(200, test.body), nil })}
		email, err := Email(context.Background(), client, "synthetic")
		if (err == nil) != test.valid || (test.valid && email != "fixture@example.com") {
			t.Fatalf("email verification result=%q err=%v", email, err)
		}
	}
}
