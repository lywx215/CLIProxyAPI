// This local-only fixture exercises the real management API against synthetic Google responses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
	access "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type fixture struct {
	mu    sync.Mutex
	gate  chan struct{}
	calls map[string]int
}

func (f *fixture) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.calls[action]++
	gate := f.gate
	f.mu.Unlock()
	response := func(raw string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
	}
	if r.Method == http.MethodGet && r.URL.Host == "vertexaisearch.cloud.google.com" {
		if action == "relogin-fixture" {
			res, _ := response(`{}`)
			res.StatusCode = http.StatusFound
			res.Header.Set("Location", "https://accounts.google.com/ServiceLogin")
			return res, nil
		}
		return response(`window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`)
	}
	if r.Method == http.MethodPost && r.URL.String() == "https://accounts.google.com/RotateCookies" {
		return response(`{}`)
	}
	if strings.HasSuffix(r.URL.Path, ":uploadFile") {
		if r.Header.Get("X-Goog-Upload-Command") == "start" {
			res, _ := response(`{}`)
			res.Header.Set("X-Goog-Upload-URL", r.URL.String()+"?upload_id=synthetic")
			return res, nil
		}
		_, _ = io.Copy(io.Discard, r.Body)
		return response(`{"fileId":"synthetic-file"}`)
	}
	switch action {
	case "lookupWidgetConfig":
		return response(`{"engineResourceDetails":{"projectNumber":"123","location":"ca","collectionId":"default_collection","engineId":"synthetic"},"anonymousWidgetConfig":{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"gemini-4-argon"},{"modelId":"gemini-3.5-flash"},{"modelId":"observed-model-id"},{"modelId":"observed-id"}]}}}}`)
	case "widgetCreateSession":
		return response(`{"session":{"name":"collections/default_collection/engines/synthetic/sessions/synthetic"}}`)
	case "widgetStreamAssist":
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return response(`[{"streamAssistResponse":{"answer":{"state":"SUCCEEDED","replies":[{"groundedContent":{"content":{"text":"Synthetic image understanding response"}}}]}}}]`)
	default:
		return nil, fmt.Errorf("fixture blocks all other network calls")
	}
}

// Clear proxy controls only on execution clones so this fixture can never reach Google.
type fixtureExecutor struct {
	*runtimeexecutor.GeminiEnterpriseWebExecutor
	transport *fixture
}

func (e *fixtureExecutor) context(ctx context.Context) context.Context {
	return context.WithValue(exec.WithoutRequestProxyURL(ctx), "cliproxy.roundtripper", http.RoundTripper(e.transport))
}
func (e *fixtureExecutor) Execute(ctx context.Context, a *auth.Auth, r exec.Request, o exec.Options) (exec.Response, error) {
	copy := a.Clone()
	copy.ProxyURL = ""
	return e.GeminiEnterpriseWebExecutor.Execute(e.context(ctx), copy, r, o)
}
func (e *fixtureExecutor) ExecuteStream(ctx context.Context, a *auth.Auth, r exec.Request, o exec.Options) (*exec.StreamResult, error) {
	copy := a.Clone()
	copy.ProxyURL = ""
	return e.GeminiEnterpriseWebExecutor.ExecuteStream(e.context(ctx), copy, r, o)
}
func (e *fixtureExecutor) Refresh(ctx context.Context, a *auth.Auth) (*auth.Auth, error) {
	copy := a.Clone()
	copy.ProxyURL = ""
	updated, err := e.GeminiEnterpriseWebExecutor.Refresh(e.context(ctx), copy)
	if updated != nil {
		updated.ProxyURL = a.ProxyURL
	}
	return updated, err
}

func (e *fixtureExecutor) CheckCookie(ctx context.Context, a *auth.Auth) bool {
	copy := a.Clone()
	copy.ProxyURL = ""
	return e.GeminiEnterpriseWebExecutor.CheckCookie(e.context(ctx), copy)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	addr := flag.String("listen", "127.0.0.1:8847", "Loopback listen address")
	frontend := flag.String("frontend", "../Cli-Proxy-API-Management-Center/dist", "Built management frontend")
	liveGoogle := flag.Bool("live-google", false, "Use real Plus upstream for explicitly authorized local verification")
	dataDirectory := flag.String("data-dir", "", "Persistent local verification directory (defaults to a temporary directory)")
	flag.Parse()
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || host != "127.0.0.1" {
		return fmt.Errorf("fixture must bind to 127.0.0.1")
	}
	dir := *dataDirectory
	if dir == "" {
		dir, err = os.MkdirTemp("", "cpa-web-fixture-*")
	} else {
		dir, err = filepath.Abs(dir)
		if err == nil {
			err = os.MkdirAll(dir, 0700)
		}
	}
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config.yaml")
	raw := fmt.Sprintf("config-version: 8\nserver: {host: 127.0.0.1, port: 8847}\noauth:\n  auth-dir: %q\nupstream:\n  gemini-enterprise-web: {enabled: false, max-concurrent: 2, max-inflight-bytes: 268435456}\n", filepath.ToSlash(filepath.Join(dir, "auths")))
	if existing, errRead := os.ReadFile(configPath); errRead == nil {
		raw = string(existing)
	} else if !os.IsNotExist(errRead) {
		return errRead
	} else if err = os.WriteFile(configPath, []byte(raw), 0600); err != nil {
		return err
	}
	cfg, err := config.ParseConfigBytes([]byte(raw))
	if err != nil {
		return err
	}
	cfg.AuthDir = filepath.Join(dir, "auths")
	if err = os.MkdirAll(cfg.AuthDir, 0700); err != nil {
		return err
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(cfg.AuthDir)
	sdkauth.RegisterTokenStore(store)
	m := auth.NewManager(store, nil, nil)
	m.SetConfig(cfg)
	f := &fixture{calls: map[string]int{}}
	makeExecutor := func(c *config.Config) {
		copy := *c
		copy.ProxyURL = ""
		m.SetOAuthModelAlias(c.OAuthModelAlias)
		executor := runtimeexecutor.NewGeminiEnterpriseWebExecutor(&copy, m.WebRuntime())
		if *liveGoogle {
			m.RegisterExecutor(executor)
		} else {
			m.RegisterExecutor(&fixtureExecutor{executor, f})
		}
	}
	makeExecutor(cfg)
	var modelConfig atomic.Pointer[config.Config]
	modelConfig.Store(cfg.CloneForRuntime())
	if err = m.Load(context.Background()); err != nil {
		return err
	}
	publish := func(_ context.Context, a *auth.Auth) error {
		if a.Provider != web.Provider {
			return nil
		}
		if a.Disabled {
			registry.GetGlobalRegistry().UnregisterClient(a.ID)
			return nil
		}
		models := cliproxy.ProjectEnterpriseWebModels(modelConfig.Load(), a)
		registry.GetGlobalRegistry().RegisterClient(a.ID, web.Provider, models)
		return nil
	}
	for _, account := range m.List() {
		if err = publish(context.Background(), account); err != nil {
			return err
		}
	}
	_ = os.Setenv("MANAGEMENT_PASSWORD", "web-fixture-key")
	server := api.NewServer(cfg, m, access.NewManager(), configPath, api.WithLocalManagementPassword("web-fixture-key"), api.WithRequestLoggerFactory(func(*config.Config, string) logging.RequestLogger {
		return logging.NewFileRequestLogger(false, filepath.Join(dir, "logs"), dir, 10)
	}), api.WithPostAuthPersistHook(publish), api.WithConfigReloadHook(func(_ context.Context, next *config.Config) {
		next.AuthDir = cfg.AuthDir
		m.SetConfig(next)
		modelConfig.Store(next.CloneForRuntime())
		makeExecutor(next)
		for _, account := range m.List() {
			_ = publish(context.Background(), account)
		}
	}))
	root := http.NewServeMux()
	root.HandleFunc("/__fixture/hold", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		if f.gate == nil {
			f.gate = make(chan struct{})
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte("held"))
	})
	root.HandleFunc("/__fixture/release", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		if f.gate != nil {
			close(f.gate)
			f.gate = nil
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte("released"))
	})
	root.HandleFunc("/__fixture/status", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(f.calls)
	})
	root.Handle("/v8/", server.Handler())
	root.Handle("/v1beta/", server.Handler())
	root.Handle("/v1/", server.Handler())
	root.Handle("/", http.FileServer(http.Dir(*frontend)))
	if !*liveGoogle {
		// Prevent update/discovery helpers from making external HTTP calls in synthetic mode.
		http.DefaultTransport = f
	}
	m.StartAutoRefresh(context.Background(), 15*time.Minute)
	defer m.StopAutoRefresh()
	fmt.Printf("Local Web verification: http://%s · live_google=%t · management password: web-fixture-key · data: %s\n", *addr, *liveGoogle, dir)
	return http.ListenAndServe(*addr, root)
}
