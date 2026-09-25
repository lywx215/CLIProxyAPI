// Command diag-integration hosts only isolated DIAG-07 fixture traffic.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type transportProvider struct{ transport *http.Transport }

func (p transportProvider) RoundTripperFor(*auth.Auth) http.RoundTripper { return p.transport }

func run() error {
	upstreams := flag.String("upstreams", "", "comma separated exact loopback HTTP origins, optionally /antigravity")
	models := flag.String("models", "gemini-2.5-flash", "synthetic model names")
	debug := flag.Bool("debug", false, "enable DEBUG")
	throttle := flag.Bool("throttle", false, "fixed 1000 token/s and 100ms first delay")
	rate := flag.Int("rate", 1000, "fixed tokens per second")
	delay := flag.Int("first-delay", 100, "fixed first response delay in milliseconds")
	flag.Parse()
	if *rate <= 0 || *delay < 0 {
		return errors.New("invalid fixture throttle configuration")
	}
	allowed := map[string]bool{}
	for _, raw := range strings.Split(*upstreams, ",") {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/antigravity") {
			return errors.New("only explicit 127.0.0.1 HTTP fixture upstreams are allowed")
		}
		allowed[u.Host] = true
	}
	// No config loader, auth store, refresh loop, or remote model updater is started.
	logging.SetupBaseLogger()
	util.SetLogLevel(&config.Config{Debug: *debug})
	gin.SetMode(gin.ReleaseMode)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if !allowed[address] {
			return nil, errors.New("fixture outbound destination denied")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	cfg := &config.Config{}
	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 2)
	manager.RegisterExecutor(executor.NewGeminiExecutor(cfg))
	manager.SetRoundTripperProvider(transportProvider{transport})
	for i, upstream := range strings.Split(*upstreams, ",") {
		id := fmt.Sprintf("synthetic-%d", i)
		if _, err := manager.Register(context.Background(), &auth.Auth{ID: id, Provider: "gemini", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "synthetic-local-password", "base_url": upstream}}); err != nil {
			return err
		}
		var infos []*registry.ModelInfo
		for _, model := range strings.Split(*models, ",") {
			infos = append(infos, &registry.ModelInfo{ID: model})
		}
		registry.GetGlobalRegistry().RegisterClient(id, "gemini", infos)
		defer registry.GetGlobalRegistry().UnregisterClient(id)
	}
	sdk := &config.SDKConfig{SpeedThrottle: config.SpeedThrottleConfig{Enabled: *throttle, MinTokensPerSecond: *rate, MaxTokensPerSecond: *rate, MinFirstTokenDelayMs: *delay, MaxFirstTokenDelayMs: *delay}}
	base := handlers.NewBaseAPIHandlers(sdk, manager)
	router := gin.New()
	router.Use(logging.GinLogrusLogger(), logging.GinDiagnostics(), gin.Recovery())
	router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(base).GeminiHandler)
	router.POST("/v1/chat/completions", openai.NewOpenAIAPIHandler(base).ChatCompletions)
	router.GET("/__health", func(c *gin.Context) { c.JSON(200, gin.H{"ready": true}) })
	router.POST("/__debug/:state", func(c *gin.Context) {
		util.SetLogLevel(&config.Config{Debug: c.Param("state") == "on"})
		c.JSON(200, gin.H{"ok": true})
	})
	server := &http.Server{Handler: router}
	stopped := make(chan error, 1)
	router.POST("/__shutdown", func(c *gin.Context) {
		c.JSON(200, gin.H{"stopping": true})
		go func() { stopped <- server.Shutdown(context.Background()) }()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"fixture": "cpa", "address": "http://" + listener.Addr().String(), "pid": os.Getpid()}); err != nil {
		return err
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return <-stopped
	}
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
