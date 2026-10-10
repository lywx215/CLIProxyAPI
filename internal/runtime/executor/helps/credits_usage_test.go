package helps

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCreditsUsageDoesNotLeakIntoNextAttempt(t *testing.T) {
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	requestContext := context.WithValue(context.Background(), "gin", ginContext)
	creditedAttempt := cliproxyauth.WithAntigravityCreditsUsage(requestContext)
	derivedContext, cancel := context.WithCancel(creditedAttempt)
	defer cancel()
	MarkCreditsUsed(derivedContext)
	if !CreditsUsed(creditedAttempt) {
		t.Fatal("parent attempt did not observe executor credits marking")
	}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	authID := t.Name()
	t.Cleanup(func() { cliproxyauth.ResetStatsByAuth(authID) })
	manager.MarkResult(creditedAttempt, cliproxyauth.Result{AuthID: authID, Provider: "antigravity", Model: "credited", Success: true})

	nextAttempt := cliproxyauth.WithAntigravityCreditsUsage(creditedAttempt)
	if CreditsUsed(nextAttempt) {
		t.Fatal("request-wide Gin marker leaked into an uncredited retry")
	}
	manager.MarkResult(nextAttempt, cliproxyauth.Result{AuthID: authID, Provider: "antigravity", Model: "uncredited", Success: true})
	stats := cliproxyauth.GetStatsByAuth(authID)
	if len(stats) != 2 || stats[0].Model != "credited" || stats[0].CreditsCount != 1 || stats[1].Model != "uncredited" || stats[1].CreditsCount != 0 {
		t.Fatalf("attempt credits and Stats disagree: %+v", stats)
	}
}
