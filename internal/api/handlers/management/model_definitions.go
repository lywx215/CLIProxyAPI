package management

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	web "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb"
)

// GetStaticModelDefinitions returns static model metadata for a given channel.
// Channel is provided via path param (:channel) or query param (?channel=...).
func (h *Handler) GetStaticModelDefinitions(c *gin.Context) {
	channel := strings.TrimSpace(c.Param("channel"))
	if channel == "" {
		channel = strings.TrimSpace(c.Query("channel"))
	}
	if c.GetBool(ConfigV8ContextKey) && strings.EqualFold(channel, ae.Provider) {
		c.JSON(200, gin.H{"channel": ae.Provider, "models": executor.AntigravityEnterpriseModels(ae.Config{}), "catalog_source": "official_default"})
		return
	}
	if c.GetBool(ConfigV8ContextKey) && strings.EqualFold(channel, web.Provider) {
		models := []*registry.ModelInfo{}
		seen := map[string]bool{}
		if h.authManager != nil {
			for _, a := range h.authManager.List() {
				if a.Provider != web.Provider {
					continue
				}
				credential, err := web.CredentialFrom(a.Metadata)
				if err != nil {
					continue
				}
				for _, model := range credential.Catalog() {
					if !seen[model.ID] {
						seen[model.ID] = true
						models = append(models, &registry.ModelInfo{ID: model.ID, DisplayName: model.DisplayName, Type: "gemini", OwnedBy: web.Provider})
					}
				}
			}
		}
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		c.JSON(200, gin.H{"channel": web.Provider, "models": models, "catalog_source": "cached"})
		return
	}
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel is required"})
		return
	}

	models := registry.GetStaticModelDefinitionsByChannel(channel)
	if models == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown channel", "channel": channel})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"channel": strings.ToLower(strings.TrimSpace(channel)),
		"models":  models,
	})
}
