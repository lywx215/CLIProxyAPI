// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bytes"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

// Source attribution is useful independently of a text span. Enterprise's
// local offsets are intentionally not reinterpreted as Gemini global offsets.
type citationCollector struct {
	sources []contract.CitationSource
	seen    map[contract.CitationSource]bool
	bytes   int
	err     error
}

func (c *citationCollector) add(raw json.RawMessage) error {
	if c.err == nil {
		c.err = c.collect(raw)
	}
	return c.err
}

func (c *citationCollector) collect(raw json.RawMessage) error {
	if len(raw) > contract.MaxCitationBytes {
		return citationLimitFailure()
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if validateStreamFrame(raw) != nil {
		return citationMalformed()
	}
	var metadata struct {
		Citations []json.RawMessage `json:"citations"`
	}
	if json.Unmarshal(raw, &metadata) != nil {
		return citationMalformed()
	}
	for _, rawSource := range metadata.Citations {
		var fields map[string]json.RawMessage
		if json.Unmarshal(rawSource, &fields) != nil || fields == nil {
			return citationMalformed()
		}
		var source contract.CitationSource
		for name, target := range map[string]*string{"uri": &source.URI, "license": &source.License} {
			if value, exists := fields[name]; exists && !streamString(value, target) {
				return citationMalformed()
			}
		}
		// A non-HTTP source or credential-bearing URI cannot be exposed under
		// this limited subset. Its title/offset/date are never synthesized.
		if !contract.ValidGroundingWebURI(source.URI) {
			continue
		}
		if !contract.ValidCitationSource(source) {
			return citationMalformed()
		}
		if c.seen[source] {
			continue
		}
		encoded, err := json.Marshal(source)
		if err != nil {
			return citationMalformed()
		}
		if len(c.sources) >= contract.MaxCitationSources || c.bytes+len(encoded)+1 > contract.MaxCitationBytes-32 {
			return citationLimitFailure()
		}
		if c.seen == nil {
			c.seen = make(map[contract.CitationSource]bool)
		}
		c.seen[source] = true
		c.sources = append(c.sources, source)
		c.bytes += len(encoded) + 1
	}
	return nil
}

func (c *citationCollector) result() (*contract.CitationMetadata, error) {
	if c.err != nil {
		return nil, c.err
	}
	if len(c.sources) == 0 {
		return nil, nil
	}
	result := &contract.CitationMetadata{CitationSources: c.sources}
	encoded, err := json.Marshal(result)
	if err != nil {
		c.err = citationMalformed()
	} else if len(encoded) > contract.MaxCitationBytes {
		c.err = citationLimitFailure()
	}
	if c.err != nil {
		return nil, c.err
	}
	return result, nil
}

func citationMalformed() error {
	return contract.Fail(502, "DATA_LOSS", "Business citation metadata is malformed")
}
func citationLimitFailure() error {
	return contract.Fail(502, "RESOURCE_EXHAUSTED", "Business citation metadata exceeds its limit")
}
