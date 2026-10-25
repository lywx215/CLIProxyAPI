package geminienterprise

import (
	"encoding/json"
	"strings"
)

// NormalizeMedia accepts only controls whose behavior the application tool can guarantee.
func NormalizeMedia(body []byte, protocol, model, kind string) (Input, error) {
	if kind != "image" && kind != "video" {
		return Input{}, Invalid("unsupported media model")
	}
	if protocol == "openai-image" && kind != "image" || (protocol == "openai-video" || protocol == "gemini-video") && kind != "video" {
		return Input{}, Invalid("media model does not support this operation")
	}
	b, err := decodeRequestObject(body, false)
	if err != nil {
		return Input{}, err
	}
	allow := func(object map[string]any, keys, prefix string) error {
		for key := range object {
			if !strings.Contains("|"+keys+"|", "|"+key+"|") {
				return invalidField(prefix+key, "is not supported for Enterprise media generation", false)
			}
		}
		return nil
	}
	var input Input
	switch protocol {
	case "openai", "gemini":
		keys := "model|messages|stream|stream_options"
		if protocol == "gemini" {
			keys = "model|contents|systemInstruction|system_instruction|generationConfig|generation_config"
		}
		if err = allow(b, keys, ""); err != nil {
			return input, err
		}
		if protocol == "openai" {
			if messages, ok := b["messages"].([]any); ok {
				for _, message := range messages {
					if m, ok := message.(map[string]any); ok {
						if err = allow(m, "role|content", "messages."); err != nil {
							return input, err
						}
					}
				}
			}
			if v, exists := b["stream_options"]; exists {
				m, ok := v.(map[string]any)
				if !ok {
					return input, invalidField("stream_options", "must be an object", false)
				}
				if err = allow(m, "include_usage", "stream_options."); err != nil {
					return input, err
				}
				if v, exists := m["include_usage"]; exists {
					if _, ok := v.(bool); !ok {
						return input, invalidField("stream_options.include_usage", "must be a boolean", false)
					}
				}
			}
		}
		for _, key := range []string{"generationConfig", "generation_config"} {
			if v, exists := b[key]; exists {
				g, ok := v.(map[string]any)
				if !ok {
					return input, invalidField(key, "must be an object", false)
				}
				if err = allow(g, "responseModalities|response_modalities|candidateCount|candidate_count", key+"."); err != nil {
					return input, err
				}
				for k, value := range g {
					if k == "candidateCount" || k == "candidate_count" {
						if value != json.Number("1") {
							return input, invalidField(key+"."+k, "must be 1", false)
						}
					} else {
						a, ok := value.([]any)
						if !ok || len(a) == 0 {
							return input, invalidField(key+"."+k, "must be an array of supported modalities", false)
						}
						for _, modality := range a {
							if modality != "TEXT" && modality != strings.ToUpper(kind) {
								return input, invalidField(key+"."+k, "contains an unsupported modality", false)
							}
						}
					}
				}
			}
		}
		input, err = Normalize(body, protocol, model)
		if err != nil {
			return input, err
		}
	case "openai-image", "openai-video", "gemini-video":
		prompt := ""
		if protocol == "gemini-video" {
			if err = allow(b, "instances|parameters", ""); err != nil {
				return input, err
			}
			a, ok := b["instances"].([]any)
			if !ok || len(a) != 1 {
				return input, invalidField("instances", "must contain exactly one text prompt", false)
			}
			m, ok := a[0].(map[string]any)
			if !ok {
				return input, invalidField("instances", "must contain an object", false)
			}
			if err = allow(m, "prompt", "instances[0]."); err != nil {
				return input, err
			}
			prompt, _ = m["prompt"].(string)
			if p, exists := b["parameters"]; exists {
				m, ok := p.(map[string]any)
				if !ok {
					return input, invalidField("parameters", "must be an object", false)
				}
				if err = allow(m, "sampleCount", "parameters."); err != nil {
					return input, err
				}
				if n, exists := m["sampleCount"]; exists && n != json.Number("1") {
					return input, invalidField("parameters.sampleCount", "must be 1", false)
				}
			}
		} else {
			keys := "model|prompt"
			if protocol == "openai-image" {
				keys += "|n|response_format|stream"
			}
			if err = allow(b, keys, ""); err != nil {
				return input, err
			}
			prompt, _ = b["prompt"].(string)
			if n, exists := b["n"]; exists && n != json.Number("1") {
				return input, invalidField("n", "must be 1", false)
			}
			if f, exists := b["response_format"]; exists && f != "b64_json" {
				return input, invalidField("response_format", "must be b64_json", false)
			}
			if s, exists := b["stream"]; exists && s != false {
				return input, invalidField("stream", "is not supported on the dedicated image endpoint", false)
			}
		}
		if strings.TrimSpace(prompt) == "" || len(prompt) > 1024*1024 {
			return input, invalidField("prompt", "must be nonempty text of at most 1 MiB", false)
		}
		input.Body = marshal(map[string]any{"query": map[string]string{"text": prompt}})
	default:
		return input, Invalid("unsupported media source protocol")
	}
	var final map[string]any
	_ = json.Unmarshal(input.Body, &final)
	delete(final, "generationSpec")
	final["toolsSpec"] = map[string]any{kind + "GenerationSpec": map[string]any{}}
	input.Body = marshal(final)
	input.MediaKind = kind
	input.Ignored = nil
	return input, nil
}

// ValidateMediaFinal is the read-only barrier after user payload rules.
func ValidateMediaFinal(body []byte, kind string) error {
	object, err := decodeRequestObject(body, true)
	if err != nil {
		return err
	}
	fields := make(map[string]fieldSpec, len(finalRequestFields))
	for k, v := range finalRequestFields {
		fields[k] = v
	}
	delete(fields, "generationSpec")
	fields["toolsSpec"] = fieldSpec{kind: "object", required: true, fields: map[string]fieldSpec{kind + "GenerationSpec": {kind: "object", required: true, fields: map[string]fieldSpec{}}}}
	if err = validateFields(object, fields, ""); err != nil {
		return err
	}
	if len(object["query"].(map[string]any)["text"].(string)) > 1024*1024 {
		return invalidField("query.text", "exceeds 1 MiB", true)
	}
	return nil
}
