package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

type enterpriseMediaAPIExecutor struct{ calls atomic.Int32 }

func (*enterpriseMediaAPIExecutor) Identifier() string { return "gemini-enterprise" }
func (e *enterpriseMediaAPIExecutor) Execute(ctx context.Context, _ *auth.Auth, req coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
	e.calls.Add(1)
	kind := registry.EnterpriseMediaKind(req.Model)
	if _, err := enterprise.NormalizeMedia(req.Payload, opts.SourceFormat.String(), req.Model, kind); err != nil {
		return coreexec.Response{}, err
	}
	data := []byte("\x00\x00\x00\x18ftypisomtest")
	return coreexec.Response{Payload: enterprise.NewOutput().MediaResponse(req.Model, opts.SourceFormat.String(), kind, "video/mp4", data)}, nil
}
func (*enterpriseMediaAPIExecutor) ExecuteStream(context.Context, *auth.Auth, coreexec.Request, coreexec.Options) (*coreexec.StreamResult, error) {
	return nil, nil
}
func (*enterpriseMediaAPIExecutor) CountTokens(context.Context, *auth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, nil
}
func (*enterpriseMediaAPIExecutor) Refresh(_ context.Context, a *auth.Auth) (*auth.Auth, error) {
	return a, nil
}
func (*enterpriseMediaAPIExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestEnterpriseAsyncVideoRoutesIdentityAndDownloads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := auth.NewManager(nil, nil, nil)
	executor := &enterpriseMediaAPIExecutor{}
	manager.RegisterExecutor(executor)
	model := "veo-3.1"
	credential := &auth.Auth{ID: "enterprise-media-api", Provider: "gemini-enterprise", Status: auth.StatusActive}
	if _, err := manager.Register(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{{ID: model, Type: "gemini", OwnedBy: "gemini-enterprise", EnterpriseMedia: "video"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager)
	t.Cleanup(func() { base.CloseMediaJobs(context.Background()) })
	o := openai.NewOpenAIAPIHandler(base)
	g := gemini.NewGeminiAPIHandler(base)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userApiKey", c.GetHeader("Authorization")) })
	router.POST("/openai/v1/videos", o.VideosCreate)
	router.GET("/openai/v1/videos/:video_id", o.VideosRetrieve)
	router.GET("/openai/v1/videos/:video_id/content", o.VideosContent)
	router.POST("/v1/videos", o.XAIVideosGenerations)
	router.POST("/v1/videos/generations", o.XAIVideosGenerations)
	router.POST("/v1beta/models/*action", g.GeminiHandler)
	router.GET("/v1beta/models/*action", g.GeminiGetHandler)
	router.GET("/v1beta/files/:file_id", g.VideoDownload)
	router.GET("/download/v1beta/files/:file_id", g.VideoDownload)
	request := func(method, path, body, owner string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", owner)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/openai/v1/videos", "/v1/videos", "/v1/videos/generations", "/v1beta/models/" + model + ":predictLongRunning"} {
		native := strings.Contains(path, "v1beta")
		body := `{"model":"` + model + `","prompt":"cat"}`
		if native {
			body = `{"instances":[{"prompt":"cat"}],"parameters":{"sampleCount":1}}`
		}
		created := request("POST", path, body, "alice")
		if created.Code != 200 {
			t.Fatalf("create %s: %d %s", path, created.Code, created.Body.String())
		}
		id := gjson.Get(created.Body.String(), "id").String()
		poll := "/openai/v1/videos/" + id
		if native {
			if !strings.HasPrefix(gjson.Get(created.Body.String(), "name").String(), "models/veo-3.1/operations/") {
				t.Fatal("native operation lost the public video model name")
			}
			poll = "/v1beta/" + gjson.Get(created.Body.String(), "name").String()
			id = poll[strings.LastIndex(poll, "/")+1:]
		} else if gjson.Get(created.Body.String(), "model").String() != "veo-3.1" {
			t.Fatal("OpenAI task lost the public video model name")
		}
		completed := false
		for i := 0; i < 1000; i++ {
			got := request("GET", poll, "", "alice")
			if got.Code != 200 {
				t.Fatalf("poll failed %d %s", got.Code, got.Body.String())
			}
			if gjson.Get(got.Body.String(), "status").String() == "failed" || gjson.Get(got.Body.String(), "error").Exists() {
				t.Fatalf("job failed: %s", got.Body.String())
			}
			if gjson.Get(got.Body.String(), "status").String() == "completed" || gjson.Get(got.Body.String(), "done").Bool() {
				if native {
					video := gjson.Get(got.Body.String(), "response.generateVideoResponse.generatedSamples.0.video")
					if gjson.Get(got.Body.String(), "name").String() != "models/veo-3.1/operations/"+id || video.Get("uri").String() != "files/"+id || video.Get("encoding").String() != "video/mp4" {
						t.Fatal("completed operation lost the Veo native SDK contract")
					}
				} else if gjson.Get(got.Body.String(), "model").String() != "veo-3.1" {
					t.Fatal("completed OpenAI task changed its public video model")
				}
				completed = true
				break
			}
			runtime.Gosched()
		}
		if !completed {
			t.Fatal("job never completed")
		}
		for _, download := range []string{"/openai/v1/videos/" + id + "/content", "/v1beta/files/" + id + ":download", "/download/v1beta/files/" + id + ":download"} {
			w := request("GET", download, "", "alice")
			if w.Code != 200 || base64.StdEncoding.EncodeToString(w.Body.Bytes()) != "AAAAGGZ0eXBpc29tdGVzdA==" {
				t.Fatalf("download %s: %d %q", download, w.Code, w.Body.String())
			}
			if request("GET", download, "", "bob").Code != 404 {
				t.Fatal("cross identity download succeeded")
			}
		}
		if request("GET", poll, "", "bob").Code != 404 {
			t.Fatal("cross identity polling succeeded")
		}
	}
	if executor.calls.Load() != 4 {
		t.Fatalf("polling/download regenerated: %d", executor.calls.Load())
	}
	if request("GET", "/v1beta/files/video_ge_missing:download", "", "alice").Code != 404 {
		t.Fatal("unknown ID did not return 404")
	}
	if request("POST", "/v1/videos", `{"model":"`+model+`","prompt":"cat","seconds":"8"}`, "alice").Code != 400 {
		t.Fatal("unsupported duration accepted")
	}
	if executor.calls.Load() != 4 {
		t.Fatal("invalid request reached generation")
	}
}
