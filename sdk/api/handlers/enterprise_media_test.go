package handlers

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/mediajobs"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

func newEnterpriseTaskHandler(t *testing.T, execute func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error)) (*BaseAPIHandler, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	model := "enterprise-task-" + strings.ToLower(t.Name())
	m := coreauth.NewManager(nil, nil, nil)
	m.RegisterExecutor(&interceptorCaptureExecutor{provider: "gemini-enterprise", execute: execute})
	m.SetRetryConfig(3, 0, 2)
	for i := 0; i < 2; i++ {
		auth := &coreauth.Auth{ID: fmt.Sprintf("%s-%d", model, i), Provider: "gemini-enterprise", Status: coreauth.StatusActive}
		if _, err := m.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model, Type: "gemini", EnterpriseMedia: "video"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, m)
	t.Cleanup(func() { h.CloseMediaJobs(context.Background()) })
	return h, model
}

func enterpriseTaskContext(owner, target string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	c.Set("userApiKey", owner)
	return c, w
}

func enterpriseTaskResource(h *BaseAPIHandler, c *gin.Context, id string, native, download bool) bool {
	handled := h.EnterpriseVideoResource(c, id, native, download)
	c.Writer.WriteHeaderNow()
	return handled
}

func awaitEnterpriseTask(t *testing.T, h *BaseAPIHandler, owner, id string) mediajobs.Task {
	t.Helper()
	c, _ := enterpriseTaskContext(owner, "/")
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		task, ok := h.videoJobs().Get(requestCallerScope(c), id)
		if !ok {
			t.Fatal("task disappeared before completion")
		}
		if !task.CompletedAt.IsZero() {
			return task
		}
		select {
		case <-deadline.C:
			t.Fatal("background task did not finish")
		case <-t.Context().Done():
			t.Fatal("test canceled before task completion")
		default:
			runtime.Gosched()
		}
	}
}

func TestEnterpriseVideoTaskDetachesRequestAndSnapshotsIdentity(t *testing.T) {
	started := make(chan struct{})
	proceed := make(chan struct{})
	observed := make(chan string, 1)
	media := []byte("\x00\x00\x00\x18ftypisomfixture")
	h, model := newEnterpriseTaskHandler(t, func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
		close(started)
		select {
		case <-ctx.Done():
			return coreexecutor.Response{}, ctx.Err()
		case <-proceed:
		}
		gc, _ := ctx.Value("gin").(*gin.Context)
		if gc == nil || ctx.Err() != nil {
			observed <- "missing snapshot or canceled background context"
		} else {
			observed <- gc.GetString("userApiKey")
		}
		return coreexecutor.Response{Payload: []byte(`{"mime_type":"video/mp4","data":"` + base64.StdEncoding.EncodeToString(media) + `"}`)}, nil
	})
	c, w := enterpriseTaskContext("alice", "/openai/v1/videos")
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = c.Request.WithContext(requestCtx)
	h.CreateEnterpriseVideo(c, model, []byte(`{"model":"`+model+`","prompt":"cat"}`), false)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := gjson.Get(w.Body.String(), "id").String()
	<-started
	pending, pendingResponse := enterpriseTaskContext("alice", "/openai/v1/videos/"+id+"/content")
	if !enterpriseTaskResource(h, pending, id, false, true) || pendingResponse.Code != http.StatusConflict {
		t.Fatalf("unfinished download: %d", pendingResponse.Code)
	}
	// A finished POST and reuse of its Gin context must not change queued work.
	cancel()
	c.Set("userApiKey", "bob")
	c.Request = httptest.NewRequest(http.MethodGet, "/reused", nil)
	close(proceed)
	if got := <-observed; got != "alice" {
		t.Fatalf("background identity = %q", got)
	}
	if task := awaitEnterpriseTask(t, h, "alice", id); task.Status != "completed" {
		t.Fatalf("task canceled with request: %s", task.Status)
	}
	operationName := "models/" + model + "/operations/" + id
	native, nativeResponse := enterpriseTaskContext("alice", "/v1beta/"+operationName)
	native.Params = gin.Params{{Key: "action", Value: "/" + model + "/operations/" + id}}
	if !enterpriseTaskResource(h, native, id, true, false) || nativeResponse.Code != http.StatusOK {
		t.Fatalf("native operation: %d", nativeResponse.Code)
	}
	nativeBody := nativeResponse.Body.String()
	video := gjson.Get(nativeBody, "response.generateVideoResponse.generatedSamples.0.video")
	if gjson.Get(nativeBody, "name").String() != operationName || !gjson.Get(nativeBody, "done").Bool() || video.Get("uri").String() != "files/"+id || video.Get("encoding").String() != "video/mp4" {
		t.Fatalf("invalid Gen AI SDK operation fields: %s", nativeBody)
	}
	for _, tc := range []struct {
		owner, query string
		status       int
	}{
		{"alice", "", http.StatusOK},
		{"alice", "?variant=video", http.StatusOK},
		{"alice", "?variant=thumbnail", http.StatusBadRequest},
		{"bob", "", http.StatusNotFound},
	} {
		download, response := enterpriseTaskContext(tc.owner, "/content"+tc.query)
		if !enterpriseTaskResource(h, download, id, false, true) || response.Code != tc.status {
			t.Fatalf("download %q %q: %d", tc.owner, tc.query, response.Code)
		}
		if tc.status == http.StatusOK && (response.Body.String() != string(media) || response.Header().Get("Content-Type") != "video/mp4" || response.Header().Get("Cache-Control") != "private, no-store") {
			t.Fatal("download bytes, MIME, or private cache header changed")
		}
	}
}

func TestEnterpriseVideoShutdownCancelsWorkAndPreventsRecreation(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h, model := newEnterpriseTaskHandler(t, func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		close(stopped)
		return coreexecutor.Response{}, ctx.Err()
	})
	c, w := enterpriseTaskContext("alice", "/openai/v1/videos")
	payload := []byte(`{"model":"` + model + `","prompt":"cat"}`)
	h.CreateEnterpriseVideo(c, model, payload, false)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	id := gjson.Get(w.Body.String(), "id").String()
	<-started
	h.CloseMediaJobs(context.Background())
	select {
	case <-stopped:
	default:
		t.Fatal("shutdown returned before active worker stopped")
	}
	if h.videoJobs() != nil {
		t.Fatal("closed handler recreated its task service")
	}
	resource, response := enterpriseTaskContext("alice", "/openai/v1/videos/"+id)
	if !enterpriseTaskResource(h, resource, id, false, false) || response.Code != http.StatusNotFound {
		t.Fatal("task remained available after shutdown")
	}
	rejected, rejection := enterpriseTaskContext("alice", "/openai/v1/videos")
	h.CreateEnterpriseVideo(rejected, model, payload, false)
	rejected.Writer.WriteHeaderNow()
	if rejection.Code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("accepted generation after shutdown: %d, calls=%d", rejection.Code, calls.Load())
	}
}

func TestEnterpriseVideoTerminalFailureDoesNotRegenerateOrExposeUpstreamIDs(t *testing.T) {
	var calls atomic.Int32
	h, model := newEnterpriseTaskHandler(t, func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
		calls.Add(1)
		return coreexecutor.Response{}, enterprise.Failure("download_failed", "private-upstream-session-file-marker", true)
	})
	c, created := enterpriseTaskContext("alice", "/v1beta/models/"+model+":predictLongRunning")
	h.CreateEnterpriseVideo(c, model, []byte(`{"instances":[{"prompt":"cat"}]}`), true)
	if created.Code != http.StatusOK {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	name := gjson.Get(created.Body.String(), "name").String()
	id := name[strings.LastIndex(name, "/")+1:]
	if task := awaitEnterpriseTask(t, h, "alice", id); task.Status != "failed" {
		t.Fatalf("status = %s", task.Status)
	}
	for i := 0; i < 3; i++ {
		poll, response := enterpriseTaskContext("alice", "/v1beta/"+name)
		poll.Params = gin.Params{{Key: "action", Value: strings.TrimPrefix(name, "models/")}}
		if !enterpriseTaskResource(h, poll, id, true, false) || response.Code != http.StatusOK {
			t.Fatalf("poll: %d", response.Code)
		}
		result := response.Body.String()
		if !gjson.Get(result, "done").Bool() || gjson.Get(result, "error.code").Int() != 13 || gjson.Get(result, "response").Exists() || strings.Contains(result, "private-upstream") {
			t.Fatalf("invalid public failed operation: %s", result)
		}
		download, notReady := enterpriseTaskContext("alice", "/content")
		if !enterpriseTaskResource(h, download, id, false, true) || notReady.Code != http.StatusConflict {
			t.Fatalf("failed download: %d", notReady.Code)
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		poll, response := enterpriseTaskContext(owner, "/v1beta/models/other/operations/"+id)
		poll.Params = gin.Params{{Key: "action", Value: "/other/operations/" + id}}
		if !enterpriseTaskResource(h, poll, id, true, false) || response.Code != http.StatusNotFound {
			t.Fatalf("forged model operation returned %d", response.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("terminal generation was retried across credentials or polling: %d calls", calls.Load())
	}
}
