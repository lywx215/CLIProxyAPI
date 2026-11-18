package management

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

const enterpriseDefinitionsV8Path = "upstream/gemini-enterprise/model-definitions"

var enterpriseDefinitionsV8Parts = []string{"upstream", "gemini-enterprise", "model-definitions"}

func readEnterpriseDefinitions(root *yaml.Node) (config.GeminiEnterpriseModels, error) {
	var value config.GeminiEnterpriseModels
	if node := configV8Node(root, enterpriseDefinitionsV8Parts); node != nil {
		err := node.Decode(&value)
		return value, err
	}
	return value, nil
}

func strictEnterpriseJSON(c *gin.Context, destination any, limit int64) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination) == nil && decoder.Decode(new(any)) == io.EOF
}

func (h *Handler) enterpriseDefinitionsV8Locked(c *gin.Context, state *configV8Document) {
	current, err := readEnterpriseDefinitions(state.Root)
	if err != nil {
		writeConfigV8Error(c, &configV8Error{Status: http.StatusInternalServerError, Code: "invalid_config"})
		return
	}
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodPut {
		if c.GetHeader("If-Match") != `"`+state.Revision+`"` {
			writeConfigV8Error(c, configV8Conflict(state.Revision))
			return
		}
		var update config.GeminiEnterpriseModels
		if !strictEnterpriseJSON(c, &update, 16*1024*1024) || update.Definitions == nil {
			writeConfigV8Error(c, &configV8Error{Status: http.StatusBadRequest, Code: "invalid_model_definitions"})
			return
		}
		if update.Revision == 0 || update.Revision != current.Revision+1 {
			writeConfigV8Error(c, &configV8Error{Status: http.StatusConflict, Code: "model_definitions_revision_conflict"})
			return
		}
		if errValidate := update.Validate(); errValidate != nil {
			writeConfigV8Error(c, &configV8Error{Status: http.StatusBadRequest, Code: "invalid_model_definitions", Message: errValidate.Error()})
			return
		}
		var node yaml.Node
		if errEncode := node.Encode(update); errEncode != nil {
			writeConfigV8Error(c, &configV8Error{Status: http.StatusBadRequest, Code: "invalid_model_definitions"})
			return
		}
		for i := len(enterpriseDefinitionsV8Parts) - 1; i >= 0; i-- {
			child := node
			node = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: enterpriseDefinitionsV8Parts[i]}, &child,
			}}
		}
		mergeConfigV8Patch(state.Root, &node)
		state.allowEnterpriseDefinitions = true
		etag, errCommit := h.commitConfigV8Locked(c.Request.Context(), state)
		if errCommit != nil {
			writeConfigV8Error(c, errCommit)
			return
		}
		c.Header("ETag", `"`+etag+`"`)
		current = update
	} else if c.Request.Method != http.MethodGet {
		c.Header("Allow", "GET, PUT")
		writeConfigV8Error(c, &configV8Error{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed"})
		return
	}
	if current.Definitions == nil {
		current.Definitions = []config.GeminiEnterpriseModelDefinition{}
	}
	c.JSON(http.StatusOK, gin.H{"revision": current.Revision,
		"applied_revision": h.authManager.AppliedEnterpriseModelDefinitionsRevision(), "definitions": current.Definitions})
}

func checkEnterpriseDefinitionsConfigWrite(state *configV8Document, next *config.Config) error {
	if state.allowEnterpriseDefinitions {
		return nil
	}
	before, err := readEnterpriseDefinitions(state.Before)
	if err != nil || !reflect.DeepEqual(before, next.GeminiEnterpriseModels) {
		return &configV8Error{Status: http.StatusBadRequest, Code: "dedicated_model_definitions_required", Field: enterpriseDefinitionsV8Path}
	}
	return nil
}
