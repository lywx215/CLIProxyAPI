package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

// ProjectText removes only media fields in contents[].parts[]. Retained JSON
// values are copied from slices, without materializing discarded attachments.
func ProjectText(ctx context.Context, body []byte, maximum int64, maxFields int, reserve func(int64) (func(), error)) ([]byte, int, bool, error) {
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, 0, false, contextError(400, "invalid_json", "body", "Request body must be valid JSON")
	}
	body = bytes.TrimSpace(body)
	p := textProjection{ctx: ctx, maximum: maximum, maxFields: maxFields, reserve: reserve}
	defer func() {
		if p.release != nil {
			p.release()
		}
	}()
	history, err := p.project(body)
	if err != nil {
		return nil, 0, false, err
	}
	if p.err != nil {
		return nil, 0, false, p.err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	return p.out, p.removed, history, nil
}

type projectionField struct {
	key, value []byte
	name       string
}

type textProjection struct {
	ctx       context.Context
	maximum   int64
	maxFields int
	reserve   func(int64) (func(), error)
	out       []byte
	release   func()
	removed   int
	err       error
}

func (p *textProjection) write(value []byte) {
	if p.err != nil {
		return
	}
	size := int64(len(p.out)) + int64(len(value))
	if size > p.maximum {
		p.err = contract.LimitFailure("body", "Text request after removing media exceeds its byte limit", contract.InputLimit{
			Name: contract.LimitRequestBytes, Observed: size, Maximum: p.maximum, AtLeast: true,
		})
		return
	}
	if size > int64(cap(p.out)) {
		capacity := min(p.maximum, max(size, 256, int64(cap(p.out))*3/2))
		release, err := p.reserve(capacity)
		if err != nil {
			p.err = err
			return
		}
		buf := make([]byte, len(p.out), int(capacity))
		copy(buf, p.out)
		if p.release != nil {
			p.release()
		}
		p.out, p.release = buf, release
	}
	p.out = append(p.out, value...)
}

// The caller keeps only the root, one content, and one part live at a time.
// Validate duplicates at the structural levels touched by the rewrite.
func (p *textProjection) object(raw []byte, path string, visit func([]projectionField) error) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if len(raw) == 0 || raw[0] != '{' {
		return contextInvalid(path, "Expected a JSON object")
	}
	var fields []projectionField
	seen := make(map[string]struct{})
	for at := skipJSONSpace(raw, 1); at < len(raw)-1; {
		if len(fields) == p.maxFields {
			return contract.LimitFailure(path, "JSON object exceeds its configured field limit", contract.InputLimit{
				Name: contract.LimitJSONObjectFields, Observed: int64(len(fields) + 1), Maximum: int64(p.maxFields), AtLeast: true,
			})
		}
		keyEnd := jsonValueEnd(raw, at)
		key := raw[at:keyEnd]
		if len(key) > 1<<20 {
			return contextInvalid(path, "JSON field name exceeds 1 MiB")
		}
		// Account for the decoded key, the field slice, and duplicate detection.
		release, err := p.reserve(2*int64(len(key)) + 256)
		if err != nil {
			return err
		}
		defer release()
		var name string
		if err = json.Unmarshal(key, &name); err != nil {
			return contextInvalid(path, "Invalid JSON field name")
		}
		if _, duplicate := seen[name]; duplicate {
			return contextInvalid(path, "JSON fields must be unique")
		}
		seen[name] = struct{}{}
		valueAt := skipJSONSpace(raw, skipJSONSpace(raw, keyEnd)+1)
		valueEnd := jsonValueEnd(raw, valueAt)
		fields = append(fields, projectionField{key: key, value: raw[valueAt:valueEnd], name: name})
		at = skipJSONSpace(raw, valueEnd)
		if raw[at] == ',' {
			at = skipJSONSpace(raw, at+1)
		}
	}
	return visit(fields)
}

func projectionValue(fields []projectionField, name string) []byte {
	for _, field := range fields {
		if field.name == name {
			return field.value
		}
	}
	return nil
}

func (p *textProjection) project(body []byte) (bool, error) {
	var history bool
	err := p.object(body, "body", func(root []projectionField) error {
		if projectionValue(root, "contents") == nil {
			return contextInvalid("contents", "Media requests require a contents array")
		}
		p.write([]byte("{"))
		for index, field := range root {
			if index > 0 {
				p.write([]byte(","))
			}
			p.write(field.key)
			p.write([]byte(":"))
			if field.name != "contents" {
				p.write(field.value)
				continue
			}
			var err error
			history, err = p.contents(field.value)
			if err != nil {
				return err
			}
		}
		p.write([]byte("}"))
		return p.err
	})
	return history, err
}

type projectionTurn struct {
	start, end int
	hasText    bool
	hasInput   bool
}

func (p *textProjection) contents(raw []byte) (bool, error) {
	// The enclosing document has already passed JSON validation.
	if len(raw) == 0 || raw[0] != '[' {
		return false, contextInvalid("contents", "Media requests require a contents array")
	}
	items := jsonArrayCursor{data: raw, at: skipJSONSpace(raw, 1)}
	var latest, previous projectionTurn
	var precedingUser, kept bool
	p.write([]byte("["))
	for index := 0; items.more(); index++ {
		path := fmt.Sprintf("contents[%d]", index)
		start := items.at
		if err := p.object(items.next(), path, func(fields []projectionField) error {
			user, err := projectionUser(fields, path)
			if err != nil {
				return err
			}
			if user && !precedingUser {
				if latest.hasText {
					previous = latest
				}
				latest = projectionTurn{start: start}
			}
			precedingUser = user
			hasParts, hasText, hasInput, err := p.content(fields, path, user, kept)
			if err != nil {
				return err
			}
			kept = kept || hasParts
			if user {
				latest.end = items.at
				latest.hasText = latest.hasText || hasText
				latest.hasInput = latest.hasInput || hasInput
			}
			return nil
		}); err != nil {
			return false, err
		}
	}
	history := !latest.hasInput
	if history {
		if !previous.hasText {
			return false, contextError(400, "media_fallback_no_text", "contents", "No user text remains after removing media. Send text with the attachment.")
		}
		if kept {
			p.write([]byte(","))
		}
		if err := p.replay(raw, previous); err != nil {
			return false, err
		}
	}
	p.write([]byte("]"))
	return history, p.err
}

func projectionUser(fields []projectionField, path string) (bool, error) {
	raw := projectionValue(fields, "role")
	if raw == nil {
		return true, nil
	}
	var role string
	if len(raw) <= 32 && json.Unmarshal(raw, &role) == nil {
		switch role {
		case "user":
			return true, nil
		case "model":
			return false, nil
		}
	}
	return false, contextInvalid(path+".role", "Role must be user or model")
}

func (p *textProjection) content(fields []projectionField, path string, user, comma bool) (kept, hasText, hasInput bool, err error) {
	raw := projectionValue(fields, "parts")
	if len(raw) == 0 || raw[0] != '[' {
		return false, false, false, contextInvalid(path+".parts", "Content parts must be an array")
	}
	parts := jsonArrayCursor{data: raw, at: skipJSONSpace(raw, 1)}
	if !parts.more() && user {
		return false, false, false, contextInvalid(path+".parts", "User input requires content parts")
	}
	start, removed := len(p.out), p.removed
	if comma {
		p.write([]byte(","))
	}
	p.write([]byte("{"))
	for index, field := range fields {
		if index > 0 {
			p.write([]byte(","))
		}
		p.write(field.key)
		p.write([]byte(":"))
		if field.name != "parts" {
			p.write(field.value)
			continue
		}
		p.write([]byte("["))
		for index := 0; parts.more(); index++ {
			err = p.object(parts.next(), fmt.Sprintf("%s.parts[%d]", path, index), func(part []projectionField) error {
				present, text, input := p.part(part, kept)
				kept = kept || present
				hasText = hasText || text
				hasInput = hasInput || input
				return p.err
			})
			if err != nil {
				return false, false, false, err
			}
		}
		p.write([]byte("]"))
	}
	p.write([]byte("}"))
	if !kept && p.removed > removed {
		p.out = p.out[:start]
		return false, false, false, p.err
	}
	return true, hasText, hasInput, p.err
}

func (p *textProjection) part(fields []projectionField, comma bool) (kept, hasText, hasInput bool) {
	removed, remaining := 0, false
	for _, field := range fields {
		switch field.name {
		case "inlineData", "inline_data", "fileData", "file_data":
			removed++
		case "thought", "thoughtSignature", "thought_signature", "mediaResolution", "media_resolution",
			"mediaProcessing", "media_processing", "videoMetadata", "video_metadata", "partMetadata", "part_metadata":
		default:
			remaining = true
		}
	}
	p.removed += removed
	if removed > 0 && !remaining {
		return false, false, false
	}
	if comma {
		p.write([]byte(","))
	}
	p.write([]byte("{"))
	for _, field := range fields {
		switch field.name {
		case "inlineData", "inline_data", "fileData", "file_data":
			continue
		case "mediaResolution", "media_resolution", "mediaProcessing", "media_processing",
			"videoMetadata", "video_metadata", "partMetadata", "part_metadata":
			if removed > 0 {
				continue
			}
		}
		if kept {
			p.write([]byte(","))
		}
		p.write(field.key)
		p.write([]byte(":"))
		p.write(field.value)
		kept = true
	}
	p.write([]byte("}"))
	if bytes.Equal(projectionValue(fields, "thought"), []byte("true")) {
		return true, false, false
	}
	hasText = visibleProjectionText(projectionValue(fields, "text"))
	hasInput = hasText
	for _, key := range []string{"functionResponse", "function_response"} {
		raw := projectionValue(fields, key)
		hasInput = hasInput || (len(raw) > 0 && raw[0] == '{')
	}
	return true, hasText, hasInput
}

// The string was already validated as JSON. Decode only enough to decide
// whether it has a non-space rune, leaving the original bytes intact.
func visibleProjectionText(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' {
		return false
	}
	for text := raw[1 : len(raw)-1]; len(text) > 0; {
		if text[0] != '\\' {
			r, n := utf8.DecodeRune(text)
			if !unicode.IsSpace(r) {
				return true
			}
			text = text[n:]
			continue
		}
		switch text[1] {
		case 't', 'r', 'n', 'f':
			text = text[2:]
		case 'u':
			value, _ := strconv.ParseUint(string(text[2:6]), 16, 16)
			if !unicode.IsSpace(rune(value)) {
				return true
			}
			text = text[6:]
		default:
			return true
		}
	}
	return false
}

func (p *textProjection) replay(raw []byte, turn projectionTurn) error {
	p.write([]byte(`{"role":"user","parts":[`))
	items := jsonArrayCursor{data: raw, at: turn.start}
	written := false
	for items.at < turn.end {
		err := p.object(items.next(), "contents", func(fields []projectionField) error {
			parts := jsonArrayCursor{data: projectionValue(fields, "parts"), at: 1}
			parts.at = skipJSONSpace(parts.data, parts.at)
			for parts.more() {
				if err := p.object(parts.next(), "contents.parts", func(part []projectionField) error {
					text := projectionValue(part, "text")
					if bytes.Equal(projectionValue(part, "thought"), []byte("true")) || !visibleProjectionText(text) {
						return p.err
					}
					if written {
						p.write([]byte(","))
					}
					p.write([]byte(`{"text":`))
					p.write(text)
					p.write([]byte("}"))
					written = true
					return p.err
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	p.write([]byte("]}"))
	return p.err
}
