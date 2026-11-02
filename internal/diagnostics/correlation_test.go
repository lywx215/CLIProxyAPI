package diagnostics

import (
	"net/http"
	"strings"
	"testing"
)

func TestCorrelationIDsKeepLocalIdentityAndExistingAliasValidation(t *testing.T) {
	e := NewEngine(ResourceConfig{}, "[]", nil, nil, func([]byte) error { return nil })
	for _, caller := range []string{"newapi-123", "a\nprivate", strings.Repeat("x", 129), ""} {
		ctx, _ := e.StartServer(t.Context(), http.Header{"X-Request-Id": {caller}}, "local-123")
		local, trace, alias := CorrelationIDs(ctx)
		if local != "local-123" || len(trace) != 32 || (caller == "newapi-123" && alias != caller) || (caller != "newapi-123" && alias != "") {
			t.Fatal("correlation changed identity ownership or alias validation")
		}
	}
	if local, trace, caller := CorrelationIDs(nil); local != "" || trace != "" || caller != "" {
		t.Fatal("absent correlation fabricated identities")
	}
}
