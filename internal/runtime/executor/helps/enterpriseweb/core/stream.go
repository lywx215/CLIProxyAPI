// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

const maxStreamBytes = 8 << 20

// decodeStream consumes one JSON envelope at a time, keeping downstream
// backpressure. Closing the array without a terminal answer is truncation.
func decodeStream(r io.Reader, emit func(contract.Event) error) error {
	reads := &streamReader{reader: r}
	decodeFailure := func(err error) error {
		if reads.err != nil {
			return &transportError{reads.err}
		}
		if reads.bytes > maxStreamBytes {
			return streamLimitFailure()
		}
		return streamDecodeFailure(err)
	}
	bounded := bufio.NewReader(io.LimitReader(reads, maxStreamBytes+1))
	for {
		prefix, err := bounded.Peek(1)
		if err != nil {
			return decodeFailure(err)
		}
		if prefix[0] == ' ' || prefix[0] == '\t' || prefix[0] == '\r' || prefix[0] == '\n' {
			_, _ = bounded.ReadByte()
			continue
		}
		if prefix[0] == '{' {
			data, err := io.ReadAll(io.LimitReader(bounded, maxErrorBody+1))
			if err != nil {
				return decodeFailure(err)
			}
			if failure, ok := rpcEnvelopeFailure(data); ok {
				return failure
			}
			return malformedStreamAt(streamInvalidEnvelope)
		}
		break
	}
	decoder := json.NewDecoder(bounded)
	token, err := decoder.Token()
	if err != nil {
		return decodeFailure(err)
	}
	if token != json.Delim('[') {
		return malformedStreamAt(streamInvalidEnvelope)
	}
	finished := false
	outputBytes := 0
	var output []contract.Part
	var grounding groundingCollector
	var citations citationCollector
	var replyGrounding replyGroundingIndex
	for decoder.More() {
		var frame struct {
			Error    json.RawMessage `json:"error"`
			Response struct {
				Answer struct {
					State   string `json:"state"`
					Replies []struct {
						ReplyID  json.RawMessage `json:"replyId"`
						Grounded *struct {
							Content               map[string]json.RawMessage `json:"content"`
							TextGroundingMetadata json.RawMessage            `json:"textGroundingMetadata"`
							CitationMetadata      json.RawMessage            `json:"citationMetadata"`
						} `json:"groundedContent"`
					} `json:"replies"`
				} `json:"answer"`
			} `json:"streamAssistResponse"`
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return decodeFailure(err)
		}
		if reads.bytes > maxStreamBytes {
			return streamLimitFailure()
		}
		// encoding/json otherwise silently replaces invalid UTF-8 and accepts
		// duplicate keys, losing opaque signatures or terminal-state fidelity.
		if err := validateStreamFrame(raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			return malformedStreamAt(streamInvalidTypedFrame)
		}
		if len(frame.Error) > 0 {
			failure := parseRPCError(frame.Error)
			failure.httpStatus = 200
			if failure.status == 0 && failure.statusEnum == "" {
				return malformedStreamAt(streamUnknownRPCError)
			}
			if failure.status == 0 {
				failure.status = httpForStatus(failure.statusEnum)
			}
			return &failure
		}
		answer := frame.Response.Answer
		if finished && (answer.State != "" || len(answer.Replies) > 0) {
			return malformedStreamAt(streamDataAfterTerminal)
		}
		parts := []contract.Part{}
		replyIDs := make([]string, 0, len(answer.Replies))
		for _, reply := range answer.Replies {
			id, err := decodeGroundingReplyID(reply.ReplyID)
			if err != nil {
				return err
			}
			replyIDs = append(replyIDs, id)
			if reply.Grounded == nil {
				return unsupportedStreamAt(streamUnsupportedReply)
			}
			if err := citations.add(reply.Grounded.CitationMetadata); err != nil {
				return err
			}
			c := reply.Grounded.Content
			if len(c) == 0 {
				// Real widget streams may carry a grounded reply without content.
				// Missing/null/empty content conveys no Part. Only the separate
				// answer state and a complete array can certify success below.
				continue
			}
			part, err := decodeStreamPart(c)
			if err != nil {
				return err
			}
			parts = append(parts, part)
			if len(parts) > maxStreamParts {
				return streamPartsLimitFailure()
			}
		}
		switch answer.State {
		case "SUCCEEDED":
			finished = true
		case "FAILED", "SKIPPED", "CANCELLED":
			return &rejectedStateError{answer.State}
		case "", "IN_PROGRESS":
		default:
			return malformedStreamAt(streamUnknownAnswerState)
		}
		projection, err := projectStreamParts(output, parts, finished)
		if err != nil {
			return err
		}
		// The same proven source-to-output mapping drives both deduplication
		// and grounding. Empty metadata has no implied previous-text anchor.
		partIndex := 0
		for _, reply := range answer.Replies {
			var text string
			var spans []streamTextSpan
			if len(reply.Grounded.Content) > 0 {
				text, spans = parts[partIndex].Text, projection.Text[partIndex]
				partIndex++
			}
			if err := grounding.addProjected(reply.Grounded.TextGroundingMetadata, text, spans); err != nil {
				return err
			}
		}
		previousParts := len(output)
		if len(projection.Parts) > 0 {
			for _, part := range projection.Parts {
				outputBytes += part.PayloadBytes()
			}
			if outputBytes > 4<<20 {
				return contract.Fail(502, "RESOURCE_EXHAUSTED", "Business output exceeds 4 MiB")
			}
			if err := emit(contract.Event{Parts: projection.Parts}); err != nil {
				return err
			}
			output = append(output, projection.Parts...)
		}
		// Keep late metadata separate until EOF. Register only Parts that were
		// successfully delivered, in original reply order, so metadata cannot
		// borrow text from a later reply or survive a later identity ambiguity.
		partIndex = 0
		for i, reply := range answer.Replies {
			if len(reply.Grounded.Content) > 0 {
				if err := replyGrounding.record(replyIDs[i], parts[partIndex], projection.Text[partIndex], previousParts); err != nil {
					return err
				}
				partIndex++
			} else if err := replyGrounding.deferMetadata(replyIDs[i], reply.Grounded.TextGroundingMetadata); err != nil {
				return err
			}
		}
	}
	token, err = decoder.Token()
	if err != nil {
		return decodeFailure(err)
	}
	if token != json.Delim(']') {
		return malformedStreamAt(streamUnexpectedClosingToken)
	}
	if !finished {
		return malformedStreamAt(streamMissingTerminalSuccess)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if reads.err != nil {
			return &transportError{reads.err}
		}
		if reads.bytes > maxStreamBytes {
			return streamLimitFailure()
		}
		return malformedStreamAt(streamTrailingData)
	}
	// LimitReader's synthetic EOF must not certify a success when trailing
	// whitespace exhausted the budget and hid the actual end of the response.
	if reads.bytes > maxStreamBytes {
		return streamLimitFailure()
	}
	if err := replyGrounding.finish(output, &grounding); err != nil {
		return err
	}
	metadata, err := grounding.result()
	if err != nil {
		return err
	}
	if metadata != nil {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return malformedStreamAt(streamInvalidGroundingMetadata)
		}
		outputBytes += len(encoded)
	}
	citationMetadata, err := citations.result()
	if err != nil {
		return err
	}
	if citationMetadata != nil {
		encoded, err := json.Marshal(citationMetadata)
		if err != nil {
			return malformedStreamAt(streamInvalidJSON)
		}
		outputBytes += len(encoded)
	}
	if outputBytes > 4<<20 {
		return contract.Fail(502, "RESOURCE_EXHAUSTED", "Business output exceeds 4 MiB")
	}
	return emit(contract.Event{Finished: true, GroundingMetadata: metadata, CitationMetadata: citationMetadata})
}

// decodeStreamPart accepts only the documented content union. Code is an
// upstream-produced value; interpreting it never executes code in this process.
func decodeStreamPart(c map[string]json.RawMessage) (contract.Part, error) {
	var part contract.Part
	values := 0
	for field := range c {
		switch field {
		case "text", "inlineData", "executableCode", "codeExecutionResult":
			values++
		case "thought", "thoughtSignature", "role":
		default:
			return part, unsupportedStreamAt(streamUnsupportedContent)
		}
	}
	if raw, ok := c["role"]; ok {
		var role string
		if json.Unmarshal(raw, &role) != nil || role != "model" {
			return part, malformedStreamAt(streamInvalidRole)
		}
	}
	if values == 0 {
		return part, malformedStreamAt(streamMissingContentValue)
	}
	if values != 1 {
		if _, inline := c["inlineData"]; inline {
			return part, malformedStreamAt(streamMixedInlinePart)
		}
		return part, malformedStreamAt(streamMixedContentPart)
	}
	if raw, ok := c["inlineData"]; ok {
		blob, err := decodeInlineData(raw)
		if err != nil {
			return part, err
		}
		// Media annotations remain attached to the same opaque binary Part.
		// Suggestions retain their established unannotated JSON envelope.
		if blob.MIMEType == contract.SuggestionsMIMEType {
			for _, field := range []string{"thought", "thoughtSignature"} {
				if _, exists := c[field]; exists {
					return part, malformedStreamAt(streamMixedInlinePart)
				}
			}
		}
		part.InlineData = blob
	} else if raw, ok := c["text"]; ok {
		if !streamString(raw, &part.Text) {
			return part, malformedStreamAt(streamInvalidText)
		}
	} else if raw, ok := c["executableCode"]; ok {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return part, malformedStreamAt(streamInvalidExecutableCode)
		}
		for name := range fields {
			if name != "code" {
				return part, unsupportedStreamAt(streamUnsupportedExecutableCode)
			}
		}
		value := &contract.ExecutableCode{Language: "PYTHON"}
		if !streamString(fields["code"], &value.Code) || value.Code == "" {
			return part, malformedStreamAt(streamInvalidExecutableCode)
		}
		part.ExecutableCode = value
	} else if raw, ok := c["codeExecutionResult"]; ok {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return part, malformedStreamAt(streamInvalidCodeExecutionResult)
		}
		for name := range fields {
			if name != "outcome" && name != "output" {
				return part, unsupportedStreamAt(streamUnsupportedCodeExecutionResult)
			}
		}
		value := &contract.CodeExecutionResult{}
		if !streamString(fields["outcome"], &value.Outcome) || !contract.ValidExecutionOutcome(value.Outcome) {
			return part, malformedStreamAt(streamInvalidCodeExecutionResult)
		}
		if raw, ok := fields["output"]; ok && !streamString(raw, &value.Output) {
			return part, malformedStreamAt(streamInvalidCodeExecutionResult)
		}
		part.CodeExecutionResult = value
	}
	if raw, ok := c["thought"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &part.Thought) != nil {
			return part, malformedStreamAt(streamInvalidThought)
		}
	}
	if raw, ok := c["thoughtSignature"]; ok && !streamString(raw, &part.ThoughtSignature) {
		return part, malformedStreamAt(streamInvalidSignature)
	}
	return part, nil
}

func streamString(raw json.RawMessage, target *string) bool {
	return len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, target) == nil
}

// Track reader failures separately from JSON decoding failures. A reset can be
// returned with the last bytes, then surface through the decoder as another
// error; it must retain its transport cause even after the closing array.
type streamReader struct {
	reader io.Reader
	err    error
	bytes  int64
}

func (r *streamReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.bytes += int64(n)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func streamLimitFailure() error {
	return contract.Fail(502, "RESOURCE_EXHAUSTED", "Business upstream stream exceeds 8 MiB")
}

func streamDecodeFailure(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var network net.Error
	if errors.As(err, &network) {
		return &transportError{err}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return malformedStreamAt(streamTruncatedJSON)
	}
	return malformedStreamAt(streamInvalidJSON)
}
