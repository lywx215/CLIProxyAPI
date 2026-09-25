package util

import (
	"context"
	"net/http"
	"testing"
)

func TestDiagnosticAutomaticHeaderReferencesVersusLiterals(t *testing.T) {
	incoming := http.Header{"Traceparent": []string{"untrusted-inbound"}, "Tracestate": []string{"vendor=inbound"}, "X-Diag-Request-Id": []string{"untrusted-id"}, "X-Business": []string{"business"}}
	attrs := map[string]string{
		"header:traceparent":       "$Traceparent",
		"header:tracestate":        "$Tracestate",
		"header:X-Diag-Request-Id": "$X-Business",
		"header:X-Alias":           "$X-Diag-Request-Id",
		"header:X-Keep":            "$X-Business",
	}
	got := extractCustomHeaders(attrs, incoming, context.Background())
	if len(got) != 1 || got["X-Keep"] != "business" {
		t.Fatalf("automatic header copies leaked: %v", got)
	}
	attrs = map[string]string{"header:traceparent": "provider-explicit", "header:tracestate": "vendor=explicit", "header:X-Request-Id": "provider-id"}
	got = extractCustomHeaders(attrs, incoming, context.Background())
	if len(got) != 3 || got["traceparent"] != "provider-explicit" {
		t.Fatal("explicit business headers removed")
	}
}
