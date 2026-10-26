package geminienterprise

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type identityTransport func(*http.Request) (*http.Response, error)

func (f identityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestEnterpriseIdentityRequiresAssignedUserLicense(t *testing.T) {
	for _, mode := range []string{"verified", "forbidden", "no-license", "other-user", "foreign-config"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: identityTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body := ""
				status := 200
				switch {
				case r.URL.Host == "www.googleapis.com":
					body = `{"email":"oauth@example.test","verified_email":true}`
				case r.URL.Host == "cloudresourcemanager.googleapis.com":
					body = `{"name":"projects/12345","projectId":"my-project","state":"ACTIVE"}`
				case strings.Contains(r.URL.Path, "/userLicenses"):
					if r.URL.Host != "us-discoveryengine.googleapis.com" {
						t.Fatal("regional license identity used the global host")
					}
					if !strings.Contains(r.URL.Path, "/userStores/default_user_store/") || r.URL.Query().Get("filter") != "" {
						t.Fatal("identity lookup guessed user store or untrusted principal")
					}
					body = `{"userLicenses":[{"userPrincipal":"oauth@example.test","licenseAssignmentState":"ASSIGNED","licenseConfig":"projects/12345/locations/us/licenseConfigs/assigned"}]}`
					switch mode {
					case "forbidden":
						status = 403
					case "no-license":
						body = `{"userLicenses":[]}`
					case "other-user":
						body = strings.ReplaceAll(body, "oauth@example.test", "other@example.test")
					case "foreign-config":
						body = strings.ReplaceAll(body, "projects/12345/", "projects/99999/")
					}
				case strings.HasSuffix(r.URL.Path, "/licenseConfigs/assigned"):
					if r.URL.Host != "us-discoveryengine.googleapis.com" {
						t.Fatal("regional subscription identity used the global host")
					}
					body = `{"name":"projects/12345/locations/us/licenseConfigs/assigned","subscriptionTier":"SEARCH_AND_ASSISTANT","state":"ACTIVE"}`
				default:
					t.Fatalf("unexpected identity endpoint: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			i := ResolveQuotaIdentity(context.Background(), client, config.GeminiEnterpriseOAuth{ClientID: "client", ProjectID: "my-project", Location: "us", EngineID: "engine"}, "synthetic", time.Now())
			if i.Verified != (mode == "verified") {
				t.Fatalf("identity trust: %+v", i)
			}
			if i.Verified && (i.ProjectNumber != "12345" || i.Edition != "enterprise_plus" || strings.Contains(i.PrincipalHash, "@")) {
				t.Fatal("identity normalization or minimization failed")
			}
			if calls == 0 {
				t.Fatal("identity inferred from metadata")
			}
		})
	}
}
