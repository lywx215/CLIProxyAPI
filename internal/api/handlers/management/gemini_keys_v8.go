package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

var geminiGroupsPath = []string{"api-keys", "gemini"}
var geminiAliasesPath = []string{"routing", "api-key-model-alias", "gemini"}

// GeminiKeyTarget addresses the persisted v8 group tree rather than runtime indexes.
type GeminiKeyTarget struct {
	GroupIndex int  `json:"groupIndex"`
	KeyIndex   *int `json:"keyIndex,omitempty"`
}

func (target *GeminiKeyTarget) UnmarshalJSON(data []byte) error {
	var value struct {
		GroupIndex *int `json:"groupIndex"`
		KeyIndex   *int `json:"keyIndex,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.GroupIndex == nil {
		return fmt.Errorf("groupIndex is required")
	}
	target.GroupIndex, target.KeyIndex = *value.GroupIndex, value.KeyIndex
	return nil
}

type geminiBatchRequest struct {
	Revision string                     `json:"revision"`
	Action   string                     `json:"action"`
	Targets  []GeminiKeyTarget          `json:"targets"`
	Changes  map[string]json.RawMessage `json:"changes,omitempty"`
}

type geminiKeyLocation struct{ groupIndex, keyIndex int }

func decodeGeminiRequest(c *gin.Context, value any) bool {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": "expected one JSON object"})
		return false
	}
	return true
}

func geminiRevisionRequired(c *gin.Context, provided, current string) bool {
	if strings.TrimSpace(provided) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "revision_required"})
		return false
	}
	if strings.TrimSpace(provided) != current {
		writeConfigV8Error(c, configV8Conflict(current))
		return false
	}
	return true
}

// GeminiAPIKeysSnapshot returns raw group inheritance with a persisted-file revision.
func (h *Handler) GeminiAPIKeysSnapshot(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	h.injectV8APIKeyAuthIndexesLocked(state.Root, state.Data)
	groups, aliases := []any{}, []any{}
	if node := configV8Node(state.Root, geminiGroupsPath); node != nil && node.Tag != "!!null" {
		if err = node.Decode(&groups); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decode_failed"})
			return
		}
	}
	if groups == nil {
		groups = []any{}
	}
	if node := configV8Node(state.Root, geminiAliasesPath); node != nil && node.Tag != "!!null" {
		if err = node.Decode(&aliases); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decode_failed"})
			return
		}
	}
	if aliases == nil {
		aliases = []any{}
	}
	c.Header("Cache-Control", "no-store")
	c.Header("ETag", `"`+state.Revision+`"`)
	c.JSON(http.StatusOK, gin.H{"revision": state.Revision, "groups": groups, "aliases": aliases,
		"capabilities": gin.H{"aliases": true, "batch": true, "migration": true}})
}

// GeminiAPIKeysBatch prevalidates every target and saves the canonical tree once.
func (h *Handler) GeminiAPIKeysBatch(c *gin.Context) {
	var request geminiBatchRequest
	if !decodeGeminiRequest(c, &request) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	if !geminiRevisionRequired(c, request.Revision, state.Revision) {
		return
	}
	affected, err := applyGeminiBatch(state.Root, request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_batch", "message": err.Error()})
		return
	}
	revision, err := h.commitConfigV8Locked(c.Request.Context(), state)
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "revision": revision, "affected": affected})
}

func geminiTargetNodes(root *yaml.Node, targets []GeminiKeyTarget) (*yaml.Node, map[int]*yaml.Node, map[geminiKeyLocation]*yaml.Node, error) {
	if len(targets) == 0 {
		return nil, nil, nil, fmt.Errorf("at least one target is required")
	}
	groups := configV8Node(root, geminiGroupsPath)
	if groups == nil || groups.Kind != yaml.SequenceNode {
		return nil, nil, nil, fmt.Errorf("Gemini groups are not configured")
	}
	selectedGroups := make(map[int]*yaml.Node)
	selectedKeys := make(map[geminiKeyLocation]*yaml.Node)
	for _, target := range targets {
		if target.GroupIndex < 0 || target.GroupIndex >= len(groups.Content) {
			return nil, nil, nil, fmt.Errorf("groupIndex %d is out of range", target.GroupIndex)
		}
		group := groups.Content[target.GroupIndex]
		keys := configV8Node(group, []string{"keys"})
		if group.Kind != yaml.MappingNode || keys == nil || keys.Kind != yaml.SequenceNode {
			return nil, nil, nil, fmt.Errorf("groupIndex %d is not a valid group", target.GroupIndex)
		}
		if target.KeyIndex == nil {
			selectedGroups[target.GroupIndex] = group
			for keyIndex, key := range keys.Content {
				selectedKeys[geminiKeyLocation{target.GroupIndex, keyIndex}] = key
			}
			continue
		}
		if *target.KeyIndex < 0 || *target.KeyIndex >= len(keys.Content) {
			return nil, nil, nil, fmt.Errorf("keyIndex %d in groupIndex %d is out of range", *target.KeyIndex, target.GroupIndex)
		}
		selectedKeys[geminiKeyLocation{target.GroupIndex, *target.KeyIndex}] = keys.Content[*target.KeyIndex]
	}
	return groups, selectedGroups, selectedKeys, nil
}

func validateGeminiBatchChanges(request geminiBatchRequest) (map[string]*yaml.Node, error) {
	if request.Action != "patch" {
		if len(request.Changes) != 0 {
			return nil, fmt.Errorf("changes are only allowed for patch")
		}
		return nil, nil
	}
	if len(request.Changes) == 0 {
		return nil, fmt.Errorf("patch requires at least one field")
	}
	changes := make(map[string]*yaml.Node, len(request.Changes))
	for field, raw := range request.Changes {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("%s: invalid value", field)
		}
		switch field {
		case "base-url", "proxy-url":
			if value != nil {
				if _, ok := value.(string); !ok {
					return nil, fmt.Errorf("%s must be a string or null", field)
				}
			}
		case "headers":
			if value != nil {
				headers, ok := value.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("headers must be an object or null")
				}
				for _, header := range headers {
					if _, ok = header.(string); !ok {
						return nil, fmt.Errorf("header values must be strings")
					}
				}
			}
		case "priority", "weight":
			if value != nil {
				var number int
				if err := json.Unmarshal(raw, &number); err != nil {
					return nil, fmt.Errorf("%s must be an integer or null", field)
				}
				if field == "weight" {
					if err := config.ValidateCredentialWeight(&number); err != nil {
						return nil, err
					}
				}
			}
		default:
			return nil, fmt.Errorf("unsupported patch field %s", field)
		}
		for _, target := range request.Targets {
			if field == "weight" && target.KeyIndex == nil {
				return nil, fmt.Errorf("weight belongs to a key")
			}
			if field == "base-url" && target.KeyIndex != nil {
				return nil, fmt.Errorf("base-url belongs to the group")
			}
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 {
			return nil, fmt.Errorf("%s: invalid value", field)
		}
		changes[field] = doc.Content[0]
	}
	return changes, nil
}

func applyGeminiBatch(root *yaml.Node, request geminiBatchRequest) (int, error) {
	switch request.Action {
	case "enable", "disable", "delete", "patch":
	default:
		return 0, fmt.Errorf("unsupported action %s", request.Action)
	}
	groups, selectedGroups, selectedKeys, err := geminiTargetNodes(root, request.Targets)
	if err != nil {
		return 0, err
	}
	changes, err := validateGeminiBatchChanges(request)
	if err != nil {
		return 0, err
	}
	// Everything above is read-only so a bad final target cannot partially mutate.
	switch request.Action {
	case "enable", "disable":
		updates := make(map[*yaml.Node]*yaml.Node, len(selectedKeys))
		for location, key := range selectedKeys {
			group := groups.Content[location.groupIndex]
			value := geminiEffectiveField(group, key, "excluded-models")
			var excluded []string
			if value != nil && value.Tag != "!!null" {
				if err = value.Decode(&excluded); err != nil {
					return 0, fmt.Errorf("invalid excluded-models")
				}
			}
			filtered := make([]string, 0, len(excluded)+1)
			for _, model := range excluded {
				if strings.TrimSpace(model) != "*" {
					filtered = append(filtered, model)
				}
			}
			if request.Action == "disable" {
				filtered = append(filtered, "*")
			}
			var node yaml.Node
			_ = node.Encode(filtered)
			updates[key] = &node
		}
		for key, node := range updates {
			setGeminiMapNode(key, "excluded-models", node)
		}
	case "patch":
		for _, target := range request.Targets {
			group := groups.Content[target.GroupIndex]
			node := group
			if target.KeyIndex != nil {
				node = selectedKeys[geminiKeyLocation{target.GroupIndex, *target.KeyIndex}]
			}
			for field, value := range changes {
				if field == "weight" && value.Tag == "!!null" {
					deleteMapKey(node, field)
				} else {
					setGeminiMapNode(node, field, value)
				}
			}
		}
	case "delete":
		for groupIndex, group := range groups.Content {
			if selectedGroups[groupIndex] != nil {
				continue
			}
			keys := configV8Node(group, []string{"keys"})
			kept := make([]*yaml.Node, 0, len(keys.Content))
			for keyIndex, key := range keys.Content {
				if selectedKeys[geminiKeyLocation{groupIndex, keyIndex}] == nil {
					kept = append(kept, key)
				}
			}
			keys.Content = kept
		}
		kept := make([]*yaml.Node, 0, len(groups.Content))
		for groupIndex, group := range groups.Content {
			if selectedGroups[groupIndex] == nil {
				kept = append(kept, group)
			}
		}
		groups.Content = kept
	}
	return len(selectedKeys), nil
}

func geminiEffectiveField(group, key *yaml.Node, field string) *yaml.Node {
	if value := configV8Node(key, []string{field}); value != nil && value.Tag != "!!null" {
		return value
	}
	return configV8Node(group, []string{field})
}

func setGeminiMapNode(mapping *yaml.Node, field string, value *yaml.Node) {
	copy := cloneConfigV8Node(value)
	if previous := configV8Node(mapping, []string{field}); previous != nil {
		copy.HeadComment, copy.LineComment, copy.FootComment = previous.HeadComment, previous.LineComment, previous.FootComment
		*previous = *copy
		return
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field}, copy)
}

func setGeminiPath(root *yaml.Node, path []string, value *yaml.Node) {
	for _, field := range path[:len(path)-1] {
		next := configV8Node(root, []string{field})
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setGeminiMapNode(root, field, next)
			next = configV8Node(root, []string{field})
		}
		root = next
	}
	setGeminiMapNode(root, path[len(path)-1], value)
}

func sortGeminiTargets(targets []GeminiKeyTarget) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].GroupIndex != targets[j].GroupIndex {
			return targets[i].GroupIndex < targets[j].GroupIndex
		}
		if targets[i].KeyIndex == nil {
			return targets[j].KeyIndex != nil
		}
		return targets[j].KeyIndex != nil && *targets[i].KeyIndex < *targets[j].KeyIndex
	})
}
