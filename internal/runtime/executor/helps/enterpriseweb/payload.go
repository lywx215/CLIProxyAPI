package enterpriseweb

import (
	"strings"

	"github.com/tidwall/gjson"
)

// SameFileIDs verifies the final payload still references exactly this request's uploads.
func SameFileIDs(body []byte, ids []string) bool {
	values := gjson.GetBytes(body, "streamAssistRequest.fileIds")
	if !values.IsArray() || len(values.Array()) != len(ids) {
		return false
	}
	for i, v := range values.Array() {
		if v.Type != gjson.String || v.String() != ids[i] {
			return false
		}
	}
	return true
}

func RequestsImageOutput(body []byte) bool {
	for _, path := range []string{"generationConfig.responseModalities", "generation_config.response_modalities"} {
		for _, value := range gjson.GetBytes(body, path).Array() {
			if strings.EqualFold(value.String(), "IMAGE") {
				return true
			}
		}
	}
	return false
}
