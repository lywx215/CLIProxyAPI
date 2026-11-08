package helps

import (
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeBusinessAICodeFunctionResponses separates client tool results from
// ordinary user parts. Business AI Code rejects mixed result/instruction turns,
// even though the native Gemini request shape permits them. Contiguous result
// parts stay together for parallel calls, and all parts retain their order.
// This must run before user payload rules and never changes tool selection.
func NormalizeBusinessAICodeFunctionResponses(body []byte) ([]byte, error) {
	contents := gjson.GetBytes(body, "contents")
	if !contents.IsArray() {
		return body, nil
	}
	items := make([][]byte, 0, len(contents.Array()))
	changed := false
	for _, content := range contents.Array() {
		parts := content.Get("parts")
		role := content.Get("role").String()
		if (role != "user" && role != "function") || !parts.IsArray() {
			items = append(items, []byte(content.Raw))
			continue
		}
		all := parts.Array()
		hasResponse, hasOther := false, false
		for _, part := range all {
			if businessAICodeFunctionResponsePart(part) {
				hasResponse = true
			} else {
				hasOther = true
			}
		}
		if !hasResponse || !hasOther {
			items = append(items, []byte(content.Raw))
			continue
		}
		for start := 0; start < len(all); {
			isResponse := businessAICodeFunctionResponsePart(all[start])
			end := start + 1
			for end < len(all) && businessAICodeFunctionResponsePart(all[end]) == isResponse {
				end++
			}
			group := make([][]byte, 0, end-start)
			for _, part := range all[start:end] {
				group = append(group, []byte(part.Raw))
			}
			turn, errSetParts := sjson.SetRawBytes([]byte(content.Raw), "parts", translatorcommon.JoinRawArray(group))
			if errSetParts != nil {
				return nil, errSetParts
			}
			items = append(items, turn)
			start = end
		}
		changed = true
	}
	if !changed {
		return body, nil
	}
	return sjson.SetRawBytes(body, "contents", translatorcommon.JoinRawArray(items))
}

func businessAICodeFunctionResponsePart(part gjson.Result) bool {
	return part.Get("functionResponse").Exists() || part.Get("function_response").Exists()
}
