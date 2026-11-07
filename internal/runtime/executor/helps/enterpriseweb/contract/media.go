// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package contract

import (
	"encoding/json"
	"mime"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValidOutputInlineMIME describes the binary pass-through subset, not the
// modalities a particular upstream model can generate. Keep the original MIME
// string in the response and history; parsing never rewrites the representation.
func ValidOutputInlineMIME(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if c < 32 || c > 126 {
			return false
		}
	}
	media, parameters, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	switch media {
	case SuggestionsMIMEType,
		"image/png", "image/jpeg", "image/webp", "image/gif", "image/heic", "image/heif",
		"audio/wav", "audio/x-wav", "audio/mp3", "audio/mpeg", "audio/mp4", "audio/aac", "audio/ogg", "audio/flac", "audio/webm",
		"video/mp4", "video/webm", "video/mpeg", "video/quicktime", "application/pdf":
		// Suggestions have a separate JSON contract, including an exact MIME.
		return len(parameters) == 0 && (media != SuggestionsMIMEType || value == SuggestionsMIMEType)
	case "audio/pcm", "audio/l16":
		// Only explicit bounded PCM metadata is understood. Do not invent rate,
		// channels or an encoding when the upstream did not provide them.
		for key, value := range parameters {
			switch key {
			case "rate", "channels":
				for _, c := range value {
					if c < '0' || c > '9' {
						return false
					}
				}
				n, err := strconv.Atoi(value)
				if err != nil || (key == "rate" && (n < 8000 || n > 192000)) || (key == "channels" && (n < 1 || n > 8)) {
					return false
				}
			case "codec":
				if value != "pcm" {
					return false
				}
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Binary bytes are opaque: never execute, decode, fetch or concatenate media.
// JSON validation applies only to the established suggestions auxiliary type.
func ValidOutputInlineData(blob *InlineData) bool {
	return blob != nil && len(blob.Data) > 0 && ValidOutputInlineMIME(blob.MIMEType) &&
		(blob.MIMEType != SuggestionsMIMEType || (utf8.Valid(blob.Data) && json.Valid(blob.Data)))
}
