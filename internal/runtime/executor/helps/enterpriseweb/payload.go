package enterpriseweb

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
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
	for _, path := range []string{"generationConfig.responseModalities", "generationConfig.response_modalities", "generation_config.responseModalities", "generation_config.response_modalities"} {
		modalities := util.GetGJSONBytesNoCopy(body, path)
		if !modalities.IsArray() {
			continue
		}
		image := false
		modalities.ForEach(func(_, value gjson.Result) bool {
			image = value.Type == gjson.String && strings.EqualFold(value.Str, "IMAGE")
			return !image
		})
		if image {
			return true
		}
	}
	return false
}
