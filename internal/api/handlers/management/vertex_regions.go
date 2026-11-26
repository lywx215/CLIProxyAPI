package management

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type vertexRegionPreset struct {
	Location      string   `json:"location"`
	DefaultModels []string `json:"default_models"`
}

type vertexModelCandidate struct {
	Model     string   `json:"model"`
	Locations []string `json:"locations"`
	RetiredAt string   `json:"retired_at,omitempty"`
}

func (h *Handler) GetVertexRegions(c *gin.Context) {
	multiRegionModels := []string{"gemini-4-neon", "gemini-4-hydrogen", "gemini-4-carbon", "gemini-4-argon"}
	argon := []string{"gemini-4-argon"}
	globalAndMulti := []string{"global", "us", "eu"}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"tested_at": "2026-11-26",
		"regions": []vertexRegionPreset{
			{Location: "global", DefaultModels: []string{}},
			{Location: "us", DefaultModels: multiRegionModels},
			{Location: "eu", DefaultModels: multiRegionModels},
			{Location: "europe-west2", DefaultModels: argon},
			{Location: "asia-southeast1", DefaultModels: argon},
		},
		"candidates": []vertexModelCandidate{
			{Model: "gemini-4-neon", Locations: globalAndMulti},
			{Model: "gemini-4-hydrogen", Locations: globalAndMulti},
			{Model: "gemini-4-carbon", Locations: globalAndMulti},
			{Model: "gemini-4-argon", Locations: []string{"global", "us", "eu", "europe-west2", "asia-southeast1"}},
			{Model: "gemini-3.1-pro-preview", Locations: []string{"global"}},
			{Model: "gemini-3.8-flash", Locations: []string{"global", "us", "eu", "northamerica-northeast1", "asia-northeast1"}},
			{Model: "gemini-2.5-pro", Locations: []string{}, RetiredAt: "2026-10-20"},
		},
	})
}

type vertexModelTestRequest struct {
	AuthIndex string `json:"auth_index"`
	Model     string `json:"model"`
}

type vertexModelTestResult struct {
	AuthIndex    string    `json:"auth_index"`
	ProjectID    string    `json:"project_id"`
	Location     string    `json:"location"`
	Model        string    `json:"model"`
	Revision     string    `json:"revision"`
	CheckedAt    time.Time `json:"checked_at"`
	ElapsedMS    int64     `json:"elapsed_ms"`
	StatusCode   int       `json:"status_code"`
	Success      bool      `json:"success"`
	GoogleStatus string    `json:"google_status,omitempty"`
	Message      string    `json:"message,omitempty"`
	ModelVersion string    `json:"model_version,omitempty"`
}

// TestVertexModel sends one text request for a saved project route, including unpublished models.
func (h *Handler) TestVertexModel(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var req vertexModelTestRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_model_test"})
		return
	}
	req.AuthIndex = strings.TrimSpace(req.AuthIndex)
	req.Model = strings.TrimSpace(req.Model)
	if req.AuthIndex == "" || !config.ValidVertexPublisherModel(req.Model) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_model_test", "message": "auth_index and a valid publisher model ID are required"})
		return
	}
	cfg, auth, revision, errTarget := h.vertexModelTestTarget(c.GetHeader("If-Match"), req.AuthIndex)
	if errTarget != nil {
		writeConfigV8Error(c, errTarget)
		return
	}
	route := auth.VertexProjectRoute()
	start := time.Now()
	result := vertexModelTestResult{
		AuthIndex: req.AuthIndex, ProjectID: route.ProjectID, Location: route.Location,
		Model: req.Model, Revision: revision, CheckedAt: start.UTC(),
	}
	testVertexGenerateContent(c.Request.Context(), cfg, auth, &result)
	result.ElapsedMS = time.Since(start).Milliseconds()
	c.Header("ETag", `"`+revision+`"`)
	c.JSON(http.StatusOK, result)
}

func (h *Handler) vertexModelTestTarget(match, index string) (*config.Config, *coreauth.Auth, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, errRead := h.readConfigV8Locked()
	if errRead != nil {
		return nil, nil, "", errRead
	}
	match = strings.TrimSpace(match)
	if match == "" {
		return nil, nil, "", &configV8Error{Status: http.StatusPreconditionRequired, Code: "config_revision_required", Revision: state.Revision}
	}
	if match != state.Revision && match != `"`+state.Revision+`"` {
		return nil, nil, "", configV8Conflict(state.Revision)
	}
	cfg, errParse := config.ParseConfigBytes(state.Data)
	if errParse != nil {
		return nil, nil, "", &configV8Error{Status: http.StatusInternalServerError, Code: "invalid_config"}
	}
	if cfg.Home.Enabled || (h.authManager != nil && h.authManager.HomeEnabled()) {
		return nil, nil, "", &configV8Error{Status: http.StatusConflict, Code: "home_managed"}
	}
	auths, errSynth := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynth != nil {
		return nil, nil, "", &configV8Error{Status: http.StatusInternalServerError, Code: "invalid_config"}
	}
	liveIndexes := liveAuthIndexFromManager(h.authManager)
	for _, auth := range auths {
		if auth.Provider != "vertex" || !auth.VertexProjectRoute().Configured() {
			continue
		}
		authIndex := liveIndexes[auth.ID]
		if authIndex == "" {
			authIndex = auth.EnsureIndex()
		}
		if authIndex == index {
			return cfg, auth, state.Revision, nil
		}
	}
	return nil, nil, "", &configV8Error{Status: http.StatusNotFound, Code: "vertex_project_route_not_found"}
}

func testVertexGenerateContent(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, result *vertexModelTestResult) {
	url, errURL := helps.VertexAPIKeyModelURL(auth.VertexProjectRoute(), "", result.Model, "generateContent")
	if errURL != nil {
		result.Message = errURL.Error()
		return
	}
	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"Reply with just the word OK."}]}],"generationConfig":{"maxOutputTokens":128}}`)
	switch result.Model {
	case "gemini-4-neon", "gemini-4-hydrogen", "gemini-4-carbon", "gemini-4-argon", "gemini-3.1-pro-preview", "gemini-3.8-flash":
		payload = []byte(`{"contents":[{"role":"user","parts":[{"text":"Reply with just the word OK."}]}],"generationConfig":{"maxOutputTokens":128,"thinkingConfig":{"thinkingLevel":"LOW"}}}`)
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if errRequest != nil {
		result.Message = "Could not build the Vertex test request"
		return
	}
	apiKey := auth.Attributes[coreauth.AttributeAPIKey]
	helps.ApplyVertexAPIKeyHeaders(req, auth, apiKey, nil)
	resp, errDo := helps.NewVertexAPIKeyHTTPClient(ctx, cfg, auth).Do(req)
	if errDo != nil {
		result.Message = "Could not reach the configured Vertex endpoint"
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("Vertex model test response body could not be closed")
		}
	}()
	result.StatusCode = resp.StatusCode
	const responseLimit = 64 * 1024
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if errRead != nil || len(body) > responseLimit {
		result.Message = "Could not read the Vertex test response"
		return
	}
	publicText := func(value string) string {
		value = strings.Join(strings.Fields(strings.ReplaceAll(value, apiKey, "[redacted]")), " ")
		runes := []rune(value)
		if len(runes) > 512 {
			return string(runes[:512])
		}
		return value
	}
	result.GoogleStatus = publicText(gjson.GetBytes(body, "error.status").String())
	if resp.StatusCode != http.StatusOK || gjson.GetBytes(body, "error").Exists() {
		result.Message = publicText(gjson.GetBytes(body, "error.message").String())
		if result.Message == "" {
			result.Message = "Vertex returned an unsuccessful test response"
		}
		return
	}
	result.ModelVersion = publicText(gjson.GetBytes(body, "modelVersion").String())
	for _, candidate := range gjson.GetBytes(body, "candidates").Array() {
		if candidate.Get("finishReason").String() != "STOP" {
			continue
		}
		for _, part := range candidate.Get("content.parts").Array() {
			text := part.Get("text")
			if !part.Get("thought").Bool() && text.Type == gjson.String && strings.TrimSpace(text.String()) != "" {
				result.Success = true
				return
			}
		}
	}
	result.Message = "Vertex returned no complete text answer"
}
