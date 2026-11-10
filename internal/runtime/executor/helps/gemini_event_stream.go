package helps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type GeminiReadResult struct{ EOF, Stopped, Residual bool }

// ReadGeminiEvents stops only between read batches. Every complete event already
// read is observed before deciding to stop; a local stop never flushes a fragment.
func ReadGeminiEvents(reader io.Reader, contentType string, now func() time.Time, consume func([]byte, time.Time) error, ready func() bool) (result GeminiReadResult, err error) {
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return readGeminiJSONEvents(reader, now, consume, ready)
	}
	var pending, event, rawTail []byte
	flush := func(at time.Time) error {
		data := bytes.TrimSpace(event)
		event = nil
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return nil
		}
		return consume(bytes.Clone(data), at)
	}
	line := func(raw []byte, at time.Time) error {
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		trimmed := bytes.TrimSpace(raw)
		// Cloud Code sometimes appends an unframed JSON error after SSE data.
		if len(rawTail) > 0 || (len(event) == 0 && bytes.HasPrefix(trimmed, []byte("{"))) {
			rawTail = append(rawTail, raw...)
			rawTail = append(rawTail, '\n')
			if len(rawTail) > StreamScannerBuffer {
				return fmt.Errorf("Gemini JSON tail exceeds size limit")
			}
			if gjson.ValidBytes(rawTail) {
				data := bytes.Clone(rawTail)
				rawTail = nil
				return consume(data, at)
			}
			return nil
		}
		if len(raw) == 0 {
			return flush(at)
		}
		if raw[0] == ':' {
			return nil
		}
		if bytes.HasPrefix(raw, []byte("data:")) {
			data := bytes.TrimPrefix(bytes.TrimPrefix(raw, []byte("data:")), []byte(" "))
			if len(event) > 0 {
				event = append(event, '\n')
			}
			event = append(event, data...)
			if len(event) > StreamScannerBuffer {
				return fmt.Errorf("Gemini SSE event exceeds size limit")
			}
		}
		return nil
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		at := now()
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				idx := bytes.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				if e := line(pending[:idx], at); e != nil {
					return result, e
				}
				pending = pending[idx+1:]
			}
			if len(pending)+len(event) > StreamScannerBuffer {
				return result, fmt.Errorf("Gemini SSE event exceeds size limit")
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				result.Residual = len(pending)+len(event)+len(rawTail) > 0
				return result, readErr
			}
			result.EOF = true
			if len(pending) > 0 {
				if e := line(pending, at); e != nil {
					return result, e
				}
			}
			if e := flush(at); e != nil {
				return result, e
			}
			if len(bytes.TrimSpace(rawTail)) > 0 {
				return result, consume(rawTail, at)
			}
			return result, nil
		}
		if ready != nil && ready() {
			result.Stopped, result.Residual = true, len(pending)+len(event)+len(rawTail) > 0
			return result, nil
		}
	}
}

// JSON transcoding uses the same batch boundary as SSE. A decoder callback must
// not stop after one array element while another buffered error remains unread.
func readGeminiJSONEvents(reader io.Reader, now func() time.Time, consume func([]byte, time.Time) error, ready func() bool) (result GeminiReadResult, err error) {
	var pending []byte
	var known, array, ended, separator, afterComma bool
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		at := now()
		pending = append(pending, buf[:n]...)
		for {
			pending = bytes.TrimSpace(pending)
			if len(pending) == 0 {
				break
			}
			if ended {
				return result, fmt.Errorf("Gemini JSON response has trailing content")
			}
			if !known {
				known, array = true, pending[0] == '['
				if array {
					pending = pending[1:]
					continue
				}
			}
			if array {
				if pending[0] == ']' && !afterComma {
					ended = true
					pending = pending[1:]
					continue
				}
				if separator {
					if pending[0] != ',' {
						return result, fmt.Errorf("invalid Gemini JSON array separator")
					}
					pending, separator, afterComma = pending[1:], false, true
					continue
				}
			}
			decoder := json.NewDecoder(bytes.NewReader(pending))
			var item json.RawMessage
			if e := decoder.Decode(&item); e != nil {
				if errors.Is(e, io.EOF) || errors.Is(e, io.ErrUnexpectedEOF) {
					break
				}
				return result, e
			}
			pending = pending[decoder.InputOffset():]
			if e := consume(item, at); e != nil {
				return result, e
			}
			separator, afterComma = array, false
		}
		if len(pending) > StreamScannerBuffer {
			return result, fmt.Errorf("Gemini JSON event exceeds size limit")
		}
		if readErr != nil {
			result.EOF = errors.Is(readErr, io.EOF)
			result.Residual = len(pending) > 0 || array && !ended
			if !result.EOF {
				return result, readErr
			}
			if result.Residual {
				return result, io.ErrUnexpectedEOF
			}
			return result, nil
		}
		if ready != nil && ready() {
			result.Stopped, result.Residual = true, len(pending) > 0 || array && !ended
			return result, nil
		}
	}
}
