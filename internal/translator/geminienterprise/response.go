package geminienterprise

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type Output struct {
	Text        string
	textRunes   int
	Parts       []map[string]any
	Sources     []Source
	Supports    []map[string]any
	Annotations []map[string]any
	lastText    string
	textBytes   int
	textRuns    []textRun
	chunks      int
	id          string
	created     int64
}

type textRun struct {
	start, end, part, runeStart int
}

func NewOutput() *Output {
	return &Output{id: "chatcmpl-" + uuid.NewString(), created: time.Now().Unix()}
}

// Add returns a delta without a synthetic role-only packet.
func (o *Output) Add(e Event) (text string, part map[string]any, err error) {
	if e.Type == "done" {
		return "", nil, nil
	}
	if e.Type == "grounding" {
		mapping := map[int]int{}
		for index, source := range e.Sources {
			i := 0
			for i < len(o.Sources) && o.Sources[i].URI != source.URI {
				i++
			}
			if i == len(o.Sources) {
				o.Sources = append(o.Sources, source)
			}
			mapping[index] = i
		}
		for _, support := range e.Supports {
			indices := make([]int, 0, len(support.Indices))
			for _, index := range support.Indices {
				indices = append(indices, mapping[index])
			}
			offset := 0
			if !e.WholeText {
				offset = o.textBytes - len(o.lastText)
			}
			for _, run := range o.textRuns {
				start := max(support.Start+offset, run.start) - run.start
				end := min(support.End+offset, run.end) - run.start
				text := o.Parts[run.part]["text"].(string)
				if start < 0 || end <= start || end > len(text) || !utf8.ValidString(text[:start]) || !utf8.ValidString(text[:end]) {
					continue
				}
				o.Supports = append(o.Supports, map[string]any{"segment": map[string]any{"partIndex": run.part, "startIndex": start, "endIndex": end, "text": text[start:end]}, "groundingChunkIndices": indices})
				// Gemini uses UTF-8 byte indices. OpenAI indices count Unicode code points.
				startRune := run.runeStart + utf8.RuneCountInString(text[:start])
				endRune := run.runeStart + utf8.RuneCountInString(text[:end])
				for _, index := range indices {
					source := o.Sources[index]
					o.Annotations = append(o.Annotations, map[string]any{"type": "url_citation", "url_citation": map[string]any{"start_index": startRune, "end_index": endRune, "url": source.URI, "title": source.Title}})
				}
			}
		}
		return "", nil, nil
	}
	switch e.Type {
	case "text":
		text = e.Text
		part = map[string]any{"text": text}
		o.lastText = text
	case "code":
		code, _ := e.Code["code"].(string)
		language, _ := e.Code["language"].(string)
		text = "\n```" + strings.ToLower(language) + "\n" + code + "\n```\n"
		part = map[string]any{"executableCode": e.Code}
	case "execution":
		output, _ := e.Execution["output"].(string)
		outcome, _ := e.Execution["outcome"].(string)
		text = "\nExecution result"
		if outcome != "" {
			text += " (" + outcome + ")"
		}
		text += ":\n```text\n" + output + "\n```\n"
		part = map[string]any{"codeExecutionResult": e.Execution}
	default:
		return "", nil, nil
	}
	o.chunks++
	if len(o.Text)+len(text) > 4*1024*1024 || o.chunks > 32768 {
		return "", nil, Failure("output_too_large", "generated output exceeded the provider limit", true)
	}
	if e.Type == "text" && len(o.Parts) > 0 {
		if previous, ok := o.Parts[len(o.Parts)-1]["text"].(string); ok {
			o.Parts[len(o.Parts)-1]["text"] = previous + text
			o.textRuns[len(o.textRuns)-1].end += len(text)
		} else {
			o.Parts = append(o.Parts, part)
			o.textRuns = append(o.textRuns, textRun{o.textBytes, o.textBytes + len(text), len(o.Parts) - 1, o.textRunes})
		}
	} else {
		o.Parts = append(o.Parts, part)
		if e.Type == "text" {
			o.textRuns = append(o.textRuns, textRun{o.textBytes, o.textBytes + len(text), len(o.Parts) - 1, o.textRunes})
		}
	}
	if e.Type == "text" {
		o.textBytes += len(text)
	}
	o.Text += text
	o.textRunes += utf8.RuneCountInString(text)
	return text, part, nil
}

func (o *Output) Grounding() map[string]any {
	if len(o.Sources) == 0 {
		return nil
	}
	chunks := make([]map[string]any, 0, len(o.Sources))
	for _, source := range o.Sources {
		chunks = append(chunks, map[string]any{"web": map[string]string{"uri": source.URI, "title": source.Title}})
	}
	m := map[string]any{"groundingChunks": chunks}
	if len(o.Supports) > 0 {
		m["groundingSupports"] = o.Supports
	}
	return m
}

func (o *Output) Finish(input Input) error {
	if strings.TrimSpace(o.Text) == "" {
		return Failure("empty_upstream", "Google returned no visible output", true)
	}
	if !input.JSON {
		return nil
	}
	var raw strings.Builder
	for _, part := range o.Parts {
		if text, ok := part["text"].(string); ok {
			raw.WriteString(text)
		}
	}
	normalized := strings.TrimSpace(raw.String())
	if strings.HasPrefix(normalized, "```") && strings.HasSuffix(normalized, "```") {
		if newline := strings.IndexByte(normalized, '\n'); newline >= 0 {
			normalized = strings.TrimSpace(normalized[newline+1 : len(normalized)-3])
		}
	}
	if !json.Valid([]byte(normalized)) {
		return Failure("invalid_json_output", "the model did not produce valid JSON", true)
	}
	if input.JSONObject && !strings.HasPrefix(normalized, "{") {
		return Failure("invalid_json_output", "json_object requires a top-level object", true)
	}
	o.Text = normalized
	o.textRunes = utf8.RuneCountInString(normalized)
	o.Parts = []map[string]any{{"text": normalized}}
	o.Supports = nil
	o.Annotations = nil
	o.Sources = nil
	return nil
}

func marshal(value any) []byte { data, _ := json.Marshal(value); return data }

func (o *Output) Gemini(done bool, parts []map[string]any) []byte {
	if parts == nil {
		parts = o.Parts
	}
	if parts == nil {
		parts = []map[string]any{}
	}
	candidate := map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}}
	if done {
		candidate["finishReason"] = "STOP"
	}
	if grounding := o.Grounding(); grounding != nil && done {
		candidate["groundingMetadata"] = grounding
	}
	return marshal(map[string]any{"candidates": []map[string]any{candidate}})
}

func (o *Output) Chat(model string) []byte {
	message := map[string]any{"role": "assistant", "content": o.Text}
	if len(o.Annotations) > 0 {
		message["annotations"] = o.Annotations
	}
	return marshal(map[string]any{"id": o.id, "object": "chat.completion", "created": o.created, "model": model, "choices": []map[string]any{{"index": 0, "message": message, "finish_reason": "stop"}}})
}

func (o *Output) ChatDelta(model, text string, annotations []map[string]any, done bool) []byte {
	delta := map[string]any{}
	var finish any
	if text != "" {
		delta["content"] = text
		delta["role"] = "assistant"
	}
	if len(annotations) > 0 {
		delta["annotations"] = annotations
	}
	if done {
		finish = "stop"
	}
	data := marshal(map[string]any{"id": o.id, "object": "chat.completion.chunk", "created": o.created, "model": model, "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}})
	return data
}
