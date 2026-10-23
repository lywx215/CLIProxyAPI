package geminienterprise

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Source struct{ URI, Title string }
type Support struct {
	Start, End int
	Text       string
	Indices    []int
}
type Event struct {
	Type      string
	Text      string
	Code      map[string]any
	Execution map[string]any
	Sources   []Source
	Supports  []Support
	WholeText bool
}

type Parser struct {
	// Error decodes sanitized structured generation errors at any stream boundary.
	Error     func([]byte, bool) error
	ReadError func(error, bool) error
	Started   bool
	terminal  string
	emit      func(Event) error
	text      strings.Builder
}

// Parse accepts JSON arrays, consecutive JSON/NDJSON, and SSE using one event parser.
// Network read boundaries never define protocol frames, and trailing corruption is fatal.
func (p *Parser) Parse(reader io.Reader, emit func(Event) error) (parseErr error) {
	tracked := &streamReadTracker{Reader: reader}
	defer func() {
		if parseErr != nil && tracked.err != nil && p.ReadError != nil {
			parseErr = p.ReadError(tracked.err, p.Started)
		}
	}()
	p.emit = emit
	limited := &io.LimitedReader{R: tracked, N: 32*1024*1024 + 1}
	r := bufio.NewReader(limited)
	for {
		ch, err := r.Peek(1)
		if err != nil {
			return p.wrap("incomplete_upstream", "stream ended without a successful terminal answer")
		}
		if bytes.ContainsAny(ch, " \t\r\n") {
			_, _ = r.ReadByte()
			continue
		}
		break
	}
	first, _ := r.Peek(1)
	var err error
	if first[0] == '[' || first[0] == '{' {
		err = p.jsonStream(r)
	} else {
		err = p.sse(r)
	}
	if err != nil {
		return err
	}
	if limited.N <= 1 {
		return p.wrap("upstream_too_large", "stream exceeded 32 MiB")
	}
	if p.terminal != "SUCCEEDED" {
		return p.wrap("incomplete_upstream", "stream ended without a successful terminal answer")
	}
	return emit(Event{Type: "done"})
}

type streamReadTracker struct {
	io.Reader
	err error
}

func (r *streamReadTracker) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func (p *Parser) wrap(kind, message string) error { return Failure(kind, message, p.Started) }
func (p *Parser) jsonStream(r io.Reader) error {
	d := json.NewDecoder(r)
	var first json.RawMessage
	// Decode one value at a time, but consume a top-level array incrementally.
	buffered := bufio.NewReader(r)
	ch, _ := buffered.Peek(1)
	d = json.NewDecoder(buffered)
	if len(ch) > 0 && ch[0] == '[' {
		if _, err := d.Token(); err != nil {
			return p.wrap("invalid_upstream_stream", "malformed JSON array")
		}
		for d.More() {
			if err := d.Decode(&first); err != nil {
				return p.wrap("invalid_upstream_stream", "malformed JSON array element")
			}
			if err := p.item(first); err != nil {
				return err
			}
		}
		if token, err := d.Token(); err != nil || token != json.Delim(']') {
			return p.wrap("invalid_upstream_stream", "unterminated JSON array")
		}
	}
	for {
		err := d.Decode(&first)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return p.wrap("invalid_upstream_stream", "malformed JSON or trailing data")
		}
		if err := p.item(first); err != nil {
			return err
		}
	}
}

func (p *Parser) sse(r io.Reader) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 16*1024*1024)
	var data []string
	event := ""
	flush := func() error {
		name := event
		event = ""
		if strings.EqualFold(name, "error") {
			var item map[string]json.RawMessage
			_ = json.Unmarshal([]byte(strings.Join(data, "\n")), &item)
			p.observeAnswerActivity(item["answer"])
			if p.Error != nil {
				return p.Error([]byte(strings.Join(data, "\n")), p.Started)
			}
			return p.wrap("upstream_error", "Google returned an SSE error event")
		}
		if len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		data = nil
		if joined == "[DONE]" {
			return nil
		}
		return p.item([]byte(joined))
	}
	for s.Scan() {
		line := s.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " ")
			continue
		}
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			continue
		}
		return p.wrap("invalid_upstream_stream", "unrecognized SSE field or trailing data")
	}
	if err := s.Err(); err != nil {
		return p.wrap("upstream_read_failed", "cannot read complete upstream stream")
	}
	return flush()
}

func (p *Parser) item(raw []byte) error {
	if !utf8.Valid(raw) {
		return p.wrap("invalid_upstream_stream", "invalid UTF-8")
	}
	if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return p.wrap("invalid_upstream_stream", "malformed SSE array")
		}
		for _, item := range items {
			if err := p.item(item); err != nil {
				return err
			}
		}
		return nil
	}
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil || item == nil {
		return p.wrap("invalid_upstream_stream", "expected upstream JSON object")
	}
	if value, ok := item["error"]; ok && string(value) != "null" {
		p.observeAnswerActivity(item["answer"])
		if p.Error != nil {
			return p.Error(raw, p.Started)
		}
		return p.wrap("upstream_error", "Google returned an error inside the generation stream")
	}
	answerRaw, ok := item["answer"]
	if code := item["code"]; len(code) > 0 && string(code) != "0" && p.Error != nil {
		return p.Error(raw, p.Started)
	}
	if !ok {
		return nil
	}
	var answerStatus struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(answerRaw, &answerStatus) == nil && len(answerStatus.Error) > 0 && string(answerStatus.Error) != "null" && p.Error != nil {
		p.observeAnswerActivity(answerRaw)
		return p.Error(answerStatus.Error, p.Started)
	}
	if p.terminal != "" {
		return p.wrap("invalid_upstream_stream", "answer received after terminal state")
	}
	var answer struct {
		State   string `json:"state"`
		Replies []struct {
			Content         json.RawMessage `json:"content"`
			GroundedContent struct {
				Content  json.RawMessage `json:"content"`
				Metadata json.RawMessage `json:"textGroundingMetadata"`
			} `json:"groundedContent"`
		} `json:"replies"`
	}
	errAnswer := json.Unmarshal(answerRaw, &answer)
	if len(answer.Replies) > 0 || answer.State == "IN_PROGRESS" {
		p.Started = true
	}
	if errAnswer != nil {
		return p.wrap("invalid_upstream_stream", "invalid answer")
	}
	for _, reply := range answer.Replies {
		content := reply.GroundedContent.Content
		if len(content) == 0 {
			content = reply.Content
		}
		if len(content) == 0 {
			if len(reply.GroundedContent.Metadata) == 0 {
				continue
			}
			content = json.RawMessage(`{}`)
		}
		var c struct {
			Thought    bool            `json:"thought"`
			Text       string          `json:"text"`
			Code       map[string]any  `json:"executableCode"`
			Execution  map[string]any  `json:"codeExecutionResult"`
			File       json.RawMessage `json:"file"`
			InlineData json.RawMessage `json:"inlineData"`
		}
		if json.Unmarshal(content, &c) != nil {
			return p.wrap("invalid_upstream_stream", "invalid reply content")
		}
		if c.Thought {
			continue
		}
		if classifyEnterpriseMedia(c.File, c.InlineData) == enterpriseMediaUnsupported {
			return p.wrap("unsupported_upstream_content", "Google returned media that cannot be represented by this text provider")
		}
		if c.Text != "" {
			if err := p.emit(Event{Type: "text", Text: c.Text}); err != nil {
				return err
			}
			p.text.WriteString(c.Text)
		}
		if len(reply.GroundedContent.Metadata) > 0 {
			text := c.Text
			wholeText := text == ""
			if wholeText {
				text = p.text.String()
			}
			event, err := grounding(reply.GroundedContent.Metadata, text)
			if err != nil {
				return p.wrap("invalid_grounding", err.Error())
			}
			event.WholeText = wholeText
			if len(event.Sources) > 0 {
				if err := p.emit(event); err != nil {
					return err
				}
			}
		}
		if c.Code != nil {
			code, ok := c.Code["code"].(string)
			if !ok {
				return p.wrap("invalid_upstream_stream", "executableCode.code is not text")
			}
			part := map[string]any{"code": code}
			if value, exists := c.Code["language"]; exists {
				language, ok := value.(string)
				if !ok {
					return p.wrap("invalid_upstream_stream", "executableCode.language is not text")
				}
				part["language"] = language
			}
			if err := p.emit(Event{Type: "code", Code: part}); err != nil {
				return err
			}
		}
		if c.Execution != nil {
			part := map[string]any{}
			for _, key := range []string{"output", "outcome"} {
				if value, exists := c.Execution[key]; exists {
					if _, ok := value.(string); !ok {
						return p.wrap("invalid_upstream_stream", "codeExecutionResult fields must be text")
					}
					part[key] = value
				}
			}
			if err := p.emit(Event{Type: "execution", Execution: part}); err != nil {
				return err
			}
		}
	}
	switch answer.State {
	case "", "IN_PROGRESS", "STATE_UNSPECIFIED":
	case "SUCCEEDED":
		p.terminal = answer.State
	default:
		p.terminal = answer.State
		return p.wrap("upstream_failed", "Google generation ended with "+answer.State)
	}
	return nil
}

func (p *Parser) observeAnswerActivity(raw json.RawMessage) {
	var activity struct {
		State   string            `json:"state"`
		Replies []json.RawMessage `json:"replies"`
	}
	_ = json.Unmarshal(raw, &activity)
	if activity.State == "IN_PROGRESS" || len(activity.Replies) > 0 {
		p.Started = true
	}
}

func upstreamIndex(v json.RawMessage) (int, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var number json.Number
	if json.Unmarshal(v, &number) != nil {
		return 0, false
	}
	i, err := strconv.ParseInt(string(number), 10, 32)
	return int(i), err == nil
}

func grounding(raw []byte, text string) (Event, error) {
	var m struct {
		References []struct {
			DocumentMetadata struct{ URI, Title string } `json:"documentMetadata"`
		} `json:"references"`
		Segments []struct {
			Start   json.RawMessage `json:"startIndex"`
			End     json.RawMessage `json:"endIndex"`
			Text    *string         `json:"text"`
			Indices []int           `json:"referenceIndices"`
		} `json:"segments"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return Event{}, fmt.Errorf("malformed grounding metadata")
	}
	e := Event{Type: "grounding"}
	remap := map[int]int{}
	for index, reference := range m.References {
		doc := reference.DocumentMetadata
		if !strings.HasPrefix(doc.URI, "https://") && !strings.HasPrefix(doc.URI, "http://") {
			continue
		}
		remap[index] = len(e.Sources)
		e.Sources = append(e.Sources, Source{URI: doc.URI, Title: doc.Title})
	}
	for _, segment := range m.Segments {
		start, okStart := upstreamIndex(segment.Start)
		if len(segment.Start) == 0 {
			start = 0
			okStart = true
		}
		end, okEnd := upstreamIndex(segment.End)
		if !okStart || !okEnd || start < 0 || end <= start || end > len(text) || !utf8.ValidString(text[:start]) || !utf8.ValidString(text[:end]) {
			continue
		}
		t := text[start:end]
		if segment.Text != nil && *segment.Text != t {
			continue
		}
		indices := make([]int, 0)
		for _, i := range segment.Indices {
			if mapped, ok := remap[i]; ok {
				indices = append(indices, mapped)
			}
		}
		if len(indices) > 0 {
			e.Supports = append(e.Supports, Support{Start: start, End: end, Text: t, Indices: indices})
		}
	}
	return e, nil
}
