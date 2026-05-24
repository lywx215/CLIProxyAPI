package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/vertex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// ImportVertexCredential handles uploading a Vertex service account JSON and saving it as an auth record.
func (h *Handler) ImportVertexCredential(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	if h.cfg.AuthDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file required"})
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to read file: %v", err)})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to read file: %v", err)})
		return
	}

	var credJSON map[string]any
	if err := json.Unmarshal(data, &credJSON); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}

	// Detect credential type
	credType, _ := credJSON["type"].(string)

	location := strings.TrimSpace(c.PostForm("location"))
	if location == "" {
		location = strings.TrimSpace(c.Query("location"))
	}
	if location == "" {
		location = "us-central1"
	}

	if credType == "authorized_user" {
		// ADC (Application Default Credentials) flow
		clientID, _ := credJSON["client_id"].(string)
		clientSecret, _ := credJSON["client_secret"].(string)
		refreshToken, _ := credJSON["refresh_token"].(string)
		if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" || strings.TrimSpace(refreshToken) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ADC missing client_id, client_secret, or refresh_token"})
			return
		}
		projectID := strings.TrimSpace(valueAsString(credJSON["quota_project_id"]))
		if projectID == "" {
			projectID = strings.TrimSpace(valueAsString(credJSON["project_id"]))
		}
		if projectID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "quota_project_id or project_id missing"})
			return
		}
		email, _ := credJSON["account"].(string)
		email = strings.TrimSpace(email)

		fileName := fmt.Sprintf("vertex-adc-%s.json", sanitizeVertexFilePart(projectID))
		label := labelForVertex(projectID, email)
		if label != "" {
			label += " (ADC)"
		}
		metadata := map[string]any{
			"type":             "vertex",
			"credential_type":  "authorized_user",
			"project_id":       projectID,
			"location":         location,
			"client_id":        clientID,
			"client_secret":    clientSecret,
			"refresh_token":    refreshToken,
			"quota_project_id": projectID,
			"email":            email,
			"label":            label,
		}
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "vertex",
			FileName: fileName,
			Label:    label,
			Metadata: metadata,
		}

		ctx := context.Background()
		if reqCtx := c.Request.Context(); reqCtx != nil {
			ctx = reqCtx
		}
		savedPath, err := h.saveTokenRecord(ctx, record)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "save_failed"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"status":     "ok",
			"auth-file":  savedPath,
			"project_id": projectID,
			"email":      email,
			"location":   location,
		})
		return
	}

	// Service Account flow (existing)
	normalizedSA, err := vertex.NormalizeServiceAccountMap(credJSON)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service account"})
		return
	}
	serviceAccount := normalizedSA

	projectID := strings.TrimSpace(valueAsString(serviceAccount["project_id"]))
	if projectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project_id missing"})
		return
	}
	email := strings.TrimSpace(valueAsString(serviceAccount["client_email"]))

	fileName := fmt.Sprintf("vertex-%s.json", sanitizeVertexFilePart(projectID))
	label := labelForVertex(projectID, email)
	storage := &vertex.VertexCredentialStorage{
		ServiceAccount: serviceAccount,
		ProjectID:      projectID,
		Email:          email,
		Location:       location,
		Type:           "vertex",
	}
	metadata := map[string]any{
		"service_account": serviceAccount,
		"project_id":      projectID,
		"email":           email,
		"location":        location,
		"type":            "vertex",
		"label":           label,
	}
	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "vertex",
		FileName: fileName,
		Storage:  storage,
		Label:    label,
		Metadata: metadata,
	}

	ctx := context.Background()
	if reqCtx := c.Request.Context(); reqCtx != nil {
		ctx = reqCtx
	}
	savedPath, err := h.saveTokenRecord(ctx, record)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save_failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"auth-file":  savedPath,
		"project_id": projectID,
		"email":      email,
		"location":   location,
	})
}

func valueAsString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func sanitizeVertexFilePart(s string) string {
	out := strings.TrimSpace(s)
	replacers := []string{"/", "_", "\\", "_", ":", "_", " ", "-"}
	for i := 0; i < len(replacers); i += 2 {
		out = strings.ReplaceAll(out, replacers[i], replacers[i+1])
	}
	if out == "" {
		return "vertex"
	}
	return out
}

func labelForVertex(projectID, email string) string {
	p := strings.TrimSpace(projectID)
	e := strings.TrimSpace(email)
	if p != "" && e != "" {
		return fmt.Sprintf("%s (%s)", p, e)
	}
	if p != "" {
		return p
	}
	if e != "" {
		return e
	}
	return "vertex"
}
