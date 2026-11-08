package helps

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeBusinessAICodeSafety removes the common Gemini translator's legacy
// category that is absent from the Business AI Code v1main HarmCategory enum.
// This runs before user payload rules; validation never rewrites their result.
func NormalizeBusinessAICodeSafety(body []byte) ([]byte, error) {
	for _, path := range []string{"safetySettings", "safety_settings"} {
		settings := gjson.GetBytes(body, path)
		if !settings.Exists() {
			continue
		}
		if !settings.IsArray() {
			return nil, errors.New("invalid Business AI Code safety settings")
		}
		items := settings.Array()
		kept := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			if item.Get("category").String() == "HARM_CATEGORY_CIVIC_INTEGRITY" {
				continue
			}
			kept = append(kept, json.RawMessage(item.Raw))
		}
		if len(kept) == len(items) {
			continue
		}
		encoded, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		body, err = sjson.SetRawBytes(body, path, encoded)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// BusinessAICodeSafetyValid checks the exact native enum identifiers after rules.
func BusinessAICodeSafetyValid(body []byte) bool {
	categoryNames := map[string]bool{"HARM_CATEGORY_UNSPECIFIED": true, "HARM_CATEGORY_HARASSMENT": true, "HARM_CATEGORY_HATE_SPEECH": true, "HARM_CATEGORY_SEXUALLY_EXPLICIT": true, "HARM_CATEGORY_DANGEROUS_CONTENT": true}
	categoryNumbers := map[string]bool{"0": true, "7": true, "8": true, "9": true, "10": true}
	thresholdNames := map[string]bool{"HARM_BLOCK_THRESHOLD_UNSPECIFIED": true, "BLOCK_LOW_AND_ABOVE": true, "BLOCK_MEDIUM_AND_ABOVE": true, "BLOCK_ONLY_HIGH": true, "BLOCK_NONE": true, "OFF": true}
	thresholdNumbers := map[string]bool{"0": true, "1": true, "2": true, "3": true, "4": true, "5": true}
	methodNames := map[string]bool{"HARM_BLOCK_METHOD_UNSPECIFIED": true, "SEVERITY": true, "PROBABILITY": true}
	methodNumbers := map[string]bool{"0": true, "1": true, "2": true}
	valid := func(value gjson.Result, names, numbers map[string]bool) bool {
		if !value.Exists() {
			return true
		}
		if value.Type == gjson.String {
			return names[value.String()]
		}
		return value.Type == gjson.Number && numbers[value.Raw]
	}
	for _, path := range []string{"safetySettings", "safety_settings"} {
		settings := gjson.GetBytes(body, path)
		if !settings.Exists() {
			continue
		}
		if !settings.IsArray() {
			return false
		}
		for _, item := range settings.Array() {
			if !item.IsObject() || !valid(item.Get("category"), categoryNames, categoryNumbers) || !valid(item.Get("threshold"), thresholdNames, thresholdNumbers) || !valid(item.Get("method"), methodNames, methodNumbers) {
				return false
			}
		}
	}
	return true
}

// AntigravityEnterpriseModels builds the catalog without probing generation.
func AntigravityEnterpriseModels(config ae.Config) []*registry.ModelInfo {
	models := ae.Models(config)
	out := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		levels := []string{"low", "medium", "high"}
		if model.Family == "gemini-3.1-pro" {
			levels = []string{"low", "high"}
		}
		out = append(out, &registry.ModelInfo{
			ID: model.ID, Object: "model", OwnedBy: ae.Provider, Type: ae.Provider,
			Name: "models/" + model.ID, MetadataModelID: model.Canonical,
			DisplayName: model.ID, Version: "v1beta", Description: "Gemini Enterprise subscription; " + model.Source,
			SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"},
			SupportedInputModalities:   []string{"TEXT", "IMAGE"}, SupportedOutputModalities: []string{"TEXT"},
			Thinking: &registry.ThinkingSupport{Levels: levels},
		})
	}
	return out
}

// BusinessAICodeJSONIsUnambiguous rejects duplicate object keys without rewriting user rules.
func BusinessAICodeJSONIsUnambiguous(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON object key")
				}
				seen[name] = true
				if err = value(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err = value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(); err != nil {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

// ReadBusinessAICodeStream decodes transport framing only. Callback owns RPC validation.
func ReadBusinessAICodeStream(reader io.Reader, contentType string, consume func([]byte) error) error {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(nil, StreamScannerBuffer)
		var event bytes.Buffer
		flush := func() error {
			if event.Len() == 0 {
				return nil
			}
			data := bytes.TrimSpace(bytes.Clone(event.Bytes()))
			event.Reset()
			if bytes.Equal(data, []byte("[DONE]")) {
				return io.EOF
			}
			return consume(data)
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				if err := flush(); err != nil {
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}
				continue
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				if event.Len() > 0 {
					event.WriteByte('\n')
				}
				event.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
				if event.Len() > StreamScannerBuffer {
					return errors.New("Business AI Code event exceeds size limit")
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		if err := flush(); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
	// Google streaming transcoding may return a JSON array instead of SSE.
	buffered := bufio.NewReader(reader)
	for {
		first, err := buffered.Peek(1)
		if err != nil {
			return err
		}
		if first[0] != ' ' && first[0] != '\n' && first[0] != '\t' && first[0] != '\r' {
			break
		}
		_, _ = buffered.ReadByte()
	}
	first, _ := buffered.Peek(1)
	decoder := json.NewDecoder(buffered)
	if first[0] == '[' {
		if _, err := decoder.Token(); err != nil {
			return err
		}
		for decoder.More() {
			var item json.RawMessage
			if err := decoder.Decode(&item); err != nil {
				return err
			}
			if err := consume(item); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return errors.New("Business AI Code response has trailing content")
		}
		return nil
	}
	for {
		var item json.RawMessage
		err := decoder.Decode(&item)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := consume(item); err != nil {
			return err
		}
	}
}
