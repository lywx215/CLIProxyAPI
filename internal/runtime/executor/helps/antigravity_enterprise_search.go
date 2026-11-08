package helps

import (
	"encoding/json"
	"reflect"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type businessAICodeSearchTools struct {
	tools        []map[string]json.RawMessage
	first        int
	standard     bool
	nativeCount  int
	nativeKey    string
	nativeConfig json.RawMessage
}

var businessAICodeSearchAliases = [...]string{
	"googleSearch", "google_search", "enterpriseWebSearch", "enterprise_web_search",
}

func invalidBusinessAICodeSearch(code string) error {
	return &ae.Error{HTTP: 400, Code: code}
}

func parseBusinessAICodeSearchTools(body []byte) (businessAICodeSearchTools, error) {
	result := businessAICodeSearchTools{first: -1}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() || !BusinessAICodeJSONIsUnambiguous(body) {
		return result, invalidBusinessAICodeSearch("invalid_search_tools")
	}
	rawTools := gjson.GetBytes(body, "tools")
	if !rawTools.Exists() || rawTools.Type == gjson.Null {
		return result, nil
	}
	if !rawTools.IsArray() {
		return result, invalidBusinessAICodeSearch("invalid_search_tools")
	}
	for index, rawTool := range rawTools.Array() {
		if !rawTool.IsObject() {
			return result, invalidBusinessAICodeSearch("invalid_search_tools")
		}
		var tool map[string]json.RawMessage
		if err := json.Unmarshal([]byte(rawTool.Raw), &tool); err != nil {
			return result, invalidBusinessAICodeSearch("invalid_search_tools")
		}
		result.tools = append(result.tools, tool)
		for aliasIndex, alias := range businessAICodeSearchAliases {
			raw, exists := tool[alias]
			if !exists {
				continue
			}
			if result.first == -1 {
				result.first = index
			}
			value := gjson.ParseBytes(raw)
			if !value.IsObject() {
				return result, invalidBusinessAICodeSearch("invalid_search_tools")
			}
			if aliasIndex < 2 {
				// Enterprise search cannot enforce the Developer API's search
				// restrictions. Reject them instead of silently broadening a query.
				if len(value.Map()) != 0 {
					return result, invalidBusinessAICodeSearch("unsupported_search_options")
				}
				result.standard = true
				continue
			}
			if result.nativeCount == 0 {
				result.nativeKey, result.nativeConfig = alias, raw
			} else {
				var previous, current any
				_ = json.Unmarshal(result.nativeConfig, &previous)
				_ = json.Unmarshal(raw, &current)
				if !reflect.DeepEqual(previous, current) {
					return result, invalidBusinessAICodeSearch("conflicting_search_options")
				}
			}
			result.nativeCount++
		}
	}
	return result, nil
}

// NormalizeBusinessAICodeSearchTools adapts the empty standard Gemini search
// declaration to Business AI Code's enterprise search. Existing native options
// remain authoritative. This must run before user payload rules, exactly once.
func NormalizeBusinessAICodeSearchTools(body []byte) ([]byte, error) {
	parsed, err := parseBusinessAICodeSearchTools(body)
	if err != nil {
		return nil, err
	}
	if !parsed.standard && parsed.nativeCount <= 1 {
		return body, nil
	}
	key, config := parsed.nativeKey, parsed.nativeConfig
	if parsed.nativeCount == 0 {
		key, config = "enterpriseWebSearch", json.RawMessage(`{}`)
	}
	tools := make([]map[string]json.RawMessage, 0, len(parsed.tools))
	for index, tool := range parsed.tools {
		removed := false
		for _, alias := range businessAICodeSearchAliases {
			if _, exists := tool[alias]; exists {
				delete(tool, alias)
				removed = true
			}
		}
		if index == parsed.first {
			tool[key] = config
		}
		if !removed || len(tool) > 0 {
			tools = append(tools, tool)
		}
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", encoded)
}

// BusinessAICodeSearchToolsValid validates the final payload without rewriting
// configured values or recreating a tool removed by payload filters.
func BusinessAICodeSearchToolsValid(body []byte) bool {
	parsed, err := parseBusinessAICodeSearchTools(body)
	return err == nil && !parsed.standard && parsed.nativeCount <= 1
}
