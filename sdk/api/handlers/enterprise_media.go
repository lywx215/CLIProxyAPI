package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/mediajobs"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	log "github.com/sirupsen/logrus"
)

func (h *BaseAPIHandler) videoJobs() *mediajobs.Manager {
	h.mediaMu.Lock()
	defer h.mediaMu.Unlock()
	if h.mediaClosed {
		return nil
	}
	if h.mediaJobs == nil {
		var ttl time.Duration
		if h.Cfg != nil {
			ttl, _ = time.ParseDuration(h.Cfg.VideoResultAuthCacheTTL)
		}
		h.mediaJobs = mediajobs.New(ttl)
	}
	return h.mediaJobs
}

func (h *BaseAPIHandler) CloseMediaJobs(ctx context.Context) {
	h.mediaMu.Lock()
	h.mediaClosed = true
	m := h.mediaJobs
	h.mediaMu.Unlock()
	if m != nil {
		m.Close(ctx)
	}
}

// CreateEnterpriseVideo snapshots the caller identity and detaches work from the POST lifetime.
func (h *BaseAPIHandler) CreateEnterpriseVideo(c *gin.Context, model string, raw []byte, native bool) {
	protocol := "openai-video"
	if native {
		protocol = "gemini-video"
	}
	if registry.EnterpriseMediaKind(model) != "video" {
		c.Status(http.StatusNotFound)
		return
	}
	input, err := enterprise.NormalizeMedia(raw, protocol, model, "video")
	if err != nil {
		h.WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: 400, Error: err})
		return
	}
	var body struct {
		Query struct {
			Text string `json:"text"`
		} `json:"query"`
	}
	_ = json.Unmarshal(input.Body, &body)
	payload, _ := json.Marshal(map[string]string{"model": model, "prompt": body.Query.Text})
	owner := requestCallerScope(c)
	// Copy selected, immutable request data only. The original Gin context is never retained.
	snapshot := &gin.Context{Request: c.Request.Clone(context.Background())}
	snapshot.Request.Body = nil
	for _, key := range []string{"userApiKey", "accessProvider"} {
		if v, ok := c.Get(key); ok {
			snapshot.Set(key, v)
		}
	}
	manager := h.videoJobs()
	if manager == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	task, err := manager.Create(owner, model, func(ctx context.Context) (mediajobs.Result, error) {
		ctx = context.WithValue(ctx, "gin", snapshot)
		resp, _, failure := h.executeWithAuthManagerFormats(ctx, "openai-video", "openai-video", model, payload, "", false, modelExecutionOptions{Operation: "video_generation"})
		if failure != nil {
			code := "upstream_error"
			var detail *enterprise.Error
			if errors.As(failure.Error, &detail) {
				code = detail.Kind
			}
			log.Warnf("Enterprise video task generation or download failed (HTTP %d; %s)", failure.StatusCode, code)
			return mediajobs.Result{}, errors.New("video generation failed")
		}
		var result struct {
			MIME string `json:"mime_type"`
			Data string `json:"data"`
		}
		if json.Unmarshal(resp, &result) != nil {
			return mediajobs.Result{}, errors.New("invalid video response")
		}
		data, errDecode := base64.StdEncoding.Strict().DecodeString(result.Data)
		return mediajobs.Result{MIME: result.MIME, Data: data}, errDecode
	})
	if err != nil {
		c.JSON(http.StatusTooManyRequests, ErrorResponse{Error: ErrorDetail{Type: "rate_limit_error", Message: "Video task queue is unavailable; retry later"}})
		return
	}
	h.writeEnterpriseVideoTask(c, task, native)
}

func (h *BaseAPIHandler) writeEnterpriseVideoTask(c *gin.Context, t mediajobs.Task, native bool) {
	if !native {
		progress := 0
		if t.Status == "completed" {
			progress = 100
		}
		body := gin.H{"id": t.ID, "object": "video", "model": t.Model, "status": t.Status, "progress": progress, "created_at": t.CreatedAt.Unix()}
		if !t.CompletedAt.IsZero() {
			body["completed_at"] = t.CompletedAt.Unix()
		}
		if t.Status == "failed" {
			body["error"] = gin.H{"code": "video_generation_failed", "message": "Video generation or download failed; generation was not retried"}
		}
		c.JSON(http.StatusOK, body)
		return
	}
	body := gin.H{"name": "models/" + t.Model + "/operations/" + t.ID, "done": !t.CompletedAt.IsZero()}
	if t.Status == "failed" {
		body["error"] = gin.H{"code": 13, "message": "Video generation or download failed; generation was not retried"}
	}
	if t.Status == "completed" {
		// A relative file resource works with SDK custom HTTP and HTTPS base URLs.
		uri := "files/" + t.ID
		body["response"] = gin.H{"generateVideoResponse": gin.H{"generatedSamples": []any{gin.H{"video": gin.H{"uri": uri, "encoding": t.Result.MIME}}}}}
	}
	c.JSON(http.StatusOK, body)
}

// EnterpriseVideoResource handles proxy-owned IDs even after eviction or a restart.
func (h *BaseAPIHandler) EnterpriseVideoResource(c *gin.Context, id string, native, download bool) bool {
	if !mediajobs.IsID(id) {
		return false
	}
	manager := h.videoJobs()
	if manager == nil {
		c.Status(http.StatusNotFound)
		return true
	}
	t, ok := manager.Get(requestCallerScope(c), id)
	if !ok {
		c.Status(http.StatusNotFound)
		return true
	}
	if native && strings.Contains(c.Param("action"), "/operations/") {
		want := t.Model + "/operations/" + id
		if strings.TrimPrefix(c.Param("action"), "/") != want {
			c.Status(http.StatusNotFound)
			return true
		}
	}
	if download {
		if t.Status != "completed" {
			c.JSON(http.StatusConflict, ErrorResponse{Error: ErrorDetail{Type: "invalid_request_error", Message: "Video is not ready"}})
			return true
		}
		if v := c.Query("variant"); v != "" && v != "video" {
			c.Status(http.StatusBadRequest)
			return true
		}
		c.Header("Cache-Control", "private, no-store")
		c.Data(http.StatusOK, t.Result.MIME, t.Result.Data)
	} else {
		h.writeEnterpriseVideoTask(c, t, native)
	}
	return true
}
