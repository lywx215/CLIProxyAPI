package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestHandlerContextCarriesDiagnosticsWithoutChangingCancellationParent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := diagnostics.NewEngine(diagnostics.ResourceConfig{}, "", nil, nil, func([]byte) error { return nil })
	requestCtx, span := e.StartServer(context.Background(), nil, "old-local-id")
	requestCtx = logging.WithRequestID(requestCtx, "old-local-id")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(requestCtx)
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	h := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, cancel := h.GetContextWithCancel(nil, c, parent)
	defer cancel()
	if diagnostics.ServerFromContext(ctx) != span || logging.GetRequestID(ctx) != "old-local-id" {
		t.Fatal("request identity was not carried")
	}
	cancelParent()
	<-ctx.Done()
	if requestCtx.Err() != nil {
		t.Fatal("request cancellation parent changed")
	}
}
