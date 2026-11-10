package enterpriseweb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Remove empty attachment placeholders. Assemble the result directly
// from raw slices so large attachments are never decoded or copied separately.
func removeEmptyAttachmentParts(body []byte) ([]byte, error) {
	contents := util.GetGJSONBytesNoCopy(body, "contents")
	if !contents.IsArray() || !mayHaveEmptyAttachment(contents.Raw) {
		return body, nil
	}
	var out []byte
	var offset int
	var err error
	contents.ForEach(func(index, content gjson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() || !mayHaveEmptyAttachment(parts.Raw) {
			return true
		}
		end := parts.Index + 1
		var removedCode string
		var kept, hasInput bool
		parts.ForEach(func(_, part gjson.Result) bool {
			raw, emptyCode := stripEmptyAttachmentPart(part)
			if emptyCode != "" && removedCode == "" {
				if out == nil {
					if !json.Valid(body) {
						err = InputFailure(http.StatusBadRequest, "invalid_json", "body", "Request body must be valid JSON")
						return false
					}
					out = make([]byte, 0, len(body))
				}
				out = append(out, body[offset:end]...)
				removedCode = emptyCode
			}
			if raw == "" {
				return true
			}
			if removedCode != "" {
				if kept {
					out = append(out, ',')
				}
				out = append(out, raw...)
			}
			kept = true
			end = part.Index + len(part.Raw)
			if !hasInput {
				text := gjson.Get(raw, "text")
				hasInput = text.Type != gjson.String || strings.TrimSpace(text.Str) != ""
			}
			return true
		})
		if err != nil || removedCode == "" {
			return err == nil
		}
		if !hasInput {
			message := "Empty inline attachments leave this turn without input. Send text or a nonempty attachment."
			if removedCode == "empty_file_data" {
				message = "Empty file attachments leave this turn without input. Send text or a nonempty file URI."
			}
			err = InputFailure(http.StatusBadRequest, removedCode, fmt.Sprintf("contents[%d].parts", index.Int()), message)
			return false
		}
		out = append(out, ']')
		offset = parts.Index + len(parts.Raw)
		return true
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return body, nil
	}
	return append(out, body[offset:]...), nil
}

func mayHaveEmptyAttachment(raw string) bool {
	return strings.Contains(raw, `""`) || strings.Contains(raw, "fileData") ||
		strings.Contains(raw, "file_data") || strings.Contains(raw, `\u`)
}

func emptyInlineAttachment(blob gjson.Result) bool {
	if !blob.IsObject() || !strings.Contains(blob.Raw, `""`) || blob.Get("data").Raw != `""` {
		return false
	}
	dataFields := 0
	blob.ForEach(func(key, _ gjson.Result) bool {
		if key.Str == "data" {
			dataFields++
		}
		return dataFields < 2
	})
	return dataFields == 1
}

func emptyFileAttachment(file gjson.Result) bool {
	if file.Raw == "null" {
		return true
	}
	if !file.IsObject() {
		return false
	}
	uriFields := 0
	empty := true
	file.ForEach(func(key, value gjson.Result) bool {
		if key.Str == "fileUri" || key.Str == "file_uri" {
			uriFields++
			empty = uriFields == 1 && (value.Raw == `""` || value.Raw == "null")
		}
		return empty
	})
	return empty
}

// Standalone attachments lose their entire part. If a client included other
// content in the same object, keep it and remove just the empty attachments.
func stripEmptyAttachmentPart(part gjson.Result) (string, string) {
	if !part.IsObject() || !mayHaveEmptyAttachment(part.Raw) {
		return part.Raw, ""
	}
	inlineKey, inline := "inlineData", part.Get("inlineData")
	if !inline.Exists() {
		inlineKey, inline = "inline_data", part.Get("inline_data")
	}
	fileKey, file := "fileData", part.Get("fileData")
	if !file.Exists() {
		fileKey, file = "file_data", part.Get("file_data")
	}
	if !emptyInlineAttachment(inline) {
		inlineKey = ""
	}
	if !emptyFileAttachment(file) {
		fileKey = ""
	}
	if inlineKey == "" && fileKey == "" {
		return part.Raw, ""
	}

	inlineFields, fileFields := 0, 0
	keep := false
	part.ForEach(func(key, _ gjson.Result) bool {
		switch key.Str {
		case "inlineData", "inline_data":
			inlineFields++
		case "fileData", "file_data":
			fileFields++
		case "thought", "thoughtSignature", "thought_signature", "partMetadata", "part_metadata",
			"mediaResolution", "media_resolution", "mediaProcessing", "media_processing", "videoMetadata", "video_metadata":
		default:
			keep = true
		}
		return true
	})
	if inlineFields != 1 {
		inlineKey = ""
	}
	if fileFields != 1 {
		fileKey = ""
	}
	if inlineKey == "" && fileKey == "" {
		return part.Raw, ""
	}
	emptyCode := "empty_inline_data"
	if fileKey != "" {
		emptyCode = "empty_file_data"
	}
	keep = keep || (inlineFields > 0 && inlineKey == "") || (fileFields > 0 && fileKey == "")
	if !keep {
		return "", emptyCode
	}
	raw := part.Raw
	for _, key := range []string{inlineKey, fileKey} {
		if key == "" {
			continue
		}
		updated, errDelete := sjson.Delete(raw, key)
		if errDelete != nil || updated == raw {
			return part.Raw, ""
		}
		raw = updated
	}
	return raw, emptyCode
}
