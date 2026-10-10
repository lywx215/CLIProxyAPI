package management

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

// ConfigV8ContextKey marks operations registered by the v8 API that save configuration.
const ConfigV8ContextKey = "management.config-v8"

// ConfigV8 serves the persisted configuration using v8 names. GET is a view;
// only a successful configuration mutation migrates the stored document.
func (h *Handler) ConfigV8(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	root, data := state.Root, state.Data
	c.Header("ETag", `"`+state.Revision+`"`)
	if c.Request.Method != http.MethodGet {
		if match := c.GetHeader("If-Match"); match != "" && !configV8RevisionMatches(match, state.Revision) {
			writeConfigV8Error(c, configV8Conflict(state.Revision))
			return
		}
	}
	path := strings.Trim(c.Param("path"), "/")
	parts := []string{}
	if path != "" {
		parts = strings.Split(path, "/")
	}
	yamlRequest := strings.HasSuffix(c.FullPath(), "/config.yaml")
	if c.Request.Method == http.MethodGet {
		config.ProjectV8ConfigAliases(root, strings.Join(parts, "."))
		if !yamlRequest {
			if servers := configV8Node(root, v8ICEServersPath); servers != nil && servers.Kind == yaml.SequenceNode {
				for _, server := range servers.Content {
					deleteConfigV8Path(server, []string{"username"})
					deleteConfigV8Path(server, []string{"credential"})
				}
			}
			h.injectV8APIKeyAuthIndexesLocked(root, data)
		}
		value := configV8Node(root, parts)
		if value == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
			return
		}
		c.Header("Cache-Control", "no-store")
		if yamlRequest {
			c.Data(http.StatusOK, "application/yaml; charset=utf-8", data)
			return
		}
		var result any
		if err = value.Decode(&result); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decode_failed"})
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}
	before := state.Before
	config.ProjectV8ConfigAliases(root, strings.Join(parts, "."))
	if c.Request.Method == http.MethodDelete {
		if len(parts) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot_delete_config"})
			return
		}
		if !deleteConfigV8Path(root, parts) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
			return
		}
	} else {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
			return
		}
		if !yamlRequest && !json.Valid(body) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
			return
		}
		var update yaml.Node
		if err = yaml.Unmarshal(body, &update); err != nil || len(update.Content) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
			return
		}
		stripAPIKeysAuthIndexesFromUpdate(parts, &update)
		if len(parts) == 0 && update.Content[0].Kind != yaml.MappingNode {
			c.JSON(http.StatusBadRequest, gin.H{"error": "config_must_be_object"})
			return
		}
		if len(parts) == 0 {
			if err = config.NormalizeV8ConfigAliases(update.Content[0]); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_config", "message": err.Error()})
				return
			}
		}
		// Paths identify YAML keys, never array indexes. Lists are replaced whole.
		dst := root
		for _, part := range parts {
			if part == "" || dst.Kind != yaml.MappingNode {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_path"})
				return
			}
			next := configV8Node(dst, []string{part})
			if next == nil {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				dst.Content = append(dst.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
			}
			dst = next
		}
		if c.Request.Method == http.MethodPatch {
			mergeConfigV8Patch(dst, update.Content[0])
		} else {
			if dst.Kind == yaml.ScalarNode && update.Content[0].Kind == yaml.ScalarNode {
				update.Content[0].HeadComment = dst.HeadComment
				update.Content[0].LineComment = dst.LineComment
				update.Content[0].FootComment = dst.FootComment
			}
			if !yamlRequest && len(parts) == 0 {
				// JSON cannot carry YAML comments, including archived unknown fields.
				// A YAML root PUT retains explicit document replacement semantics.
				preserveConfigV8JSONComments(update.Content[0], dst)
			}
			*dst = *update.Content[0]
		}
	}
	if !yamlRequest && c.Request.Method != http.MethodDelete {
		preserveV8TURNSecrets(root, before)
	}
	revision, err := h.commitConfigV8Locked(c.Request.Context(), state)
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	c.Header("ETag", `"`+revision+`"`)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "config-version": 8, "revision": revision})
}

func preserveConfigV8JSONComments(dst, src *yaml.Node) {
	if dst == nil || src == nil {
		return
	}
	dst.HeadComment, dst.LineComment, dst.FootComment = src.HeadComment, src.LineComment, src.FootComment
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		return
	}
	for index := 0; index+1 < len(dst.Content); index += 2 {
		for previous := 0; previous+1 < len(src.Content); previous += 2 {
			if dst.Content[index].Value != src.Content[previous].Value {
				continue
			}
			preserveConfigV8JSONComments(dst.Content[index], src.Content[previous])
			preserveConfigV8JSONComments(dst.Content[index+1], src.Content[previous+1])
			break
		}
	}
}

var v8ICEServersPath = []string{"oauth", "providers", "codex", "live-media-relay", "ice-servers"}

// JSON reads redact TURN secrets. Preserve omitted credentials when a client
// writes that JSON back, matching by endpoint rather than array position so a
// changed URL cannot inherit credentials for a different server. Explicit empty
// strings/null clear a secret; YAML writes retain full replacement semantics.
func preserveV8TURNSecrets(root, before *yaml.Node) {
	next := configV8Node(root, v8ICEServersPath)
	previous := configV8Node(before, v8ICEServersPath)
	if next == nil || previous == nil || next.Kind != yaml.SequenceNode || previous.Kind != yaml.SequenceNode {
		return
	}
	matched := make([]bool, len(previous.Content))
	for _, server := range next.Content {
		urls := configV8Node(server, []string{"urls"})
		if urls == nil {
			continue
		}
		var newURLs []string
		if urls.Decode(&newURLs) != nil {
			continue
		}
		for i, old := range previous.Content {
			if matched[i] {
				continue
			}
			oldURLs := configV8Node(old, []string{"urls"})
			var previousURLs []string
			if oldURLs == nil || oldURLs.Decode(&previousURLs) != nil || !reflect.DeepEqual(newURLs, previousURLs) {
				continue
			}
			matched[i] = true
			for _, name := range []string{"username", "credential"} {
				if configV8Node(server, []string{name}) == nil {
					if secret := configV8Node(old, []string{name}); secret != nil {
						server.Content = append(server.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, cloneConfigV8Node(secret))
					}
				}
			}
			break
		}
	}
}

func configV8Node(root *yaml.Node, parts []string) *yaml.Node {
	for _, part := range parts {
		if root == nil || root.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == part {
				next = root.Content[i+1]
				break
			}
		}
		root = next
	}
	return root
}

// deleteConfigV8Path prunes only empty ancestors of the removed field. Other
// explicit empty maps can carry inheritance or plugin semantics and stay intact.
func deleteConfigV8Path(root *yaml.Node, parts []string) bool {
	if root == nil || root.Kind != yaml.MappingNode || len(parts) == 0 {
		return false
	}
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != parts[0] {
			continue
		}
		if len(parts) > 1 {
			child := root.Content[i+1]
			if !deleteConfigV8Path(child, parts[1:]) {
				return false
			}
			if len(child.Content) > 0 {
				return true
			}
		}
		root.Content = append(root.Content[:i], root.Content[i+2:]...)
		return true
	}
	return false
}

func cloneConfigV8Node(node *yaml.Node) *yaml.Node {
	copy := *node
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy.Content[i] = cloneConfigV8Node(child)
	}
	return &copy
}

// Unlike JSON merge-patch, null is retained: optional key overrides use it to
// inherit the group value. DELETE is the explicit field-removal operation.
func mergeConfigV8Patch(dst, src *yaml.Node) {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		if dst.Kind == yaml.ScalarNode && src.Kind == yaml.ScalarNode {
			copy := *src
			copy.HeadComment, copy.LineComment, copy.FootComment = dst.HeadComment, dst.LineComment, dst.FootComment
			*dst = copy
			return
		}
		*dst = *src
		return
	}
	for i := 0; i < len(src.Content); i += 2 {
		key, value := src.Content[i], src.Content[i+1]
		if old := configV8Node(dst, []string{key.Value}); old != nil {
			mergeConfigV8Patch(old, value)
		} else {
			dst.Content = append(dst.Content, key, value)
		}
	}
}
