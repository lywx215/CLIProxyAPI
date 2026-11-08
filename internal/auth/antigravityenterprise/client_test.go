package antigravityenterprise

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type metadataTransport func(*http.Request) (*http.Response, error)

func (f metadataTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMetadataRPCErrorIsNotDefaultCatalogSuccess(t *testing.T) {
	client := &http.Client{Transport: metadataTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":7,"status":"PERMISSION_DENIED"}}`))}, nil
	})}
	_, err := FetchConfig(context.Background(), client, "fixture", License{ProjectID: "project", Location: "global", UserTier: "gcp-ge-plus-tier"})
	failure, ok := err.(*Error)
	if !ok || failure.HTTP != 403 || failure.Attempted {
		t.Fatalf("metadata RPC error became catalog success: %v", err)
	}
}

func TestOversizedDiscoveryCannotBePartialSuccess(t *testing.T) {
	body := `{"licenses":[]}` + strings.Repeat(" ", 4<<20)
	client := &http.Client{Transport: metadataTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if _, _, err := FetchLicenses(context.Background(), client, "fixture", ""); err == nil {
		t.Fatal("oversized truncated resource list was accepted")
	}
}
