// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
)

// parseGoogleSearch accepts the empty Google Search tool by itself. Reject
// restrictions and combinations rather than broaden a requested search.
func parseGoogleSearch(ctx context.Context, raw json.RawMessage) (bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false, nil
	}
	tools, err := rawArray(raw, 64)
	if err != nil {
		return false, err
	}
	for _, value := range tools {
		tool, err := objectContext(ctx, value)
		if err != nil {
			return false, err
		}
		search, exists, err := alias(tool, "googleSearch", "google_search")
		if err != nil {
			return false, err
		}
		if !exists {
			continue
		}
		if len(tools) != 1 || len(tool) != 1 {
			return false, invalid("Google Search must be the only tool on Gemini Enterprise Web")
		}
		options, err := objectContext(ctx, search)
		if err != nil {
			return false, err
		}
		if len(options) != 0 {
			return false, invalid("Gemini Enterprise Web supports only an empty Google Search configuration")
		}
		return true, nil
	}
	return false, nil
}

// ToolConfig can carry retrieval location/language restrictions shared by all
// tools. An empty-only adapter must not discard those and broaden the search.
func validateGoogleSearchToolConfig(ctx context.Context, root map[string]json.RawMessage) error {
	raw, exists, err := alias(root, "toolConfig", "tool_config")
	if err != nil {
		return err
	}
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	config, err := objectContext(ctx, raw)
	if err != nil {
		return err
	}
	if len(config) != 0 {
		return invalid("Gemini Enterprise Web does not support toolConfig options with Google Search")
	}
	return nil
}
