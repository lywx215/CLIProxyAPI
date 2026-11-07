package enterpriseweb

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"unicode"
)

type probeKey struct{}
type modelProbe struct {
	authID, fingerprint, model string
	epoch                      uint64
	current                    func() bool
}

type ModelProbeError struct{ Code string }

func (e *ModelProbeError) Error() string       { return "Web model test binding changed" }
func (*ModelProbeError) StatusCode() int       { return 409 }
func (*ModelProbeError) IsRequestScoped() bool { return true }

func WithModelProbe(ctx context.Context, authID, fingerprint, model string, epoch uint64, current func() bool) context.Context {
	return context.WithValue(ctx, probeKey{}, modelProbe{authID, fingerprint, model, epoch, current})
}

func isModelProbe(ctx context.Context) bool {
	_, ok := ctx.Value(probeKey{}).(modelProbe)
	return ok
}

func ValidateModelProbe(ctx context.Context, authID string, c Credential, model string, epoch uint64) error {
	probe, ok := ctx.Value(probeKey{}).(modelProbe)
	if !ok {
		return nil
	}
	if probe.authID != authID || probe.fingerprint != c.Fingerprint() || probe.epoch != epoch || (probe.current != nil && !probe.current()) {
		return &ModelProbeError{Code: "credential_changed"}
	}
	if probe.model != model {
		return &ModelProbeError{Code: "model_changed"}
	}
	return nil
}

// NewModelProbe uses a private random code and unlabeled pixels, never prompt answers.
func NewModelProbe() ([]byte, string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, "", Fail(500, "Model test randomness is unavailable")
	}
	code := "WEB-" + hex.EncodeToString(random[:])
	img := image.NewRGBA(image.Rect(0, 0, 180, 90))
	for i, c := range []color.RGBA{{230, 25, 35, 255}, {30, 60, 235, 255}, {20, 170, 50, 255}} {
		draw.Draw(img, image.Rect(i*60, 0, (i+1)*60, 90), &image.Uniform{C: c}, image.Point{}, draw.Src)
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		return nil, "", Fail(500, "Model test image could not be prepared")
	}
	inline := func(mime string, b []byte) any {
		return map[string]any{"inlineData": map[string]string{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(b)}}
	}
	parts := []any{map[string]string{"text": "Read the verification code from the attached text file and identify the colors of the three image blocks from left to right. Reply only CODE|COLOR1|COLOR2|COLOR3, using lowercase English color names. Do not guess if either attachment is unavailable."}, inline("image/png", buffer.Bytes()), inline("text/plain", []byte("verification_code: "+code+"\n"))}
	b, err := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}})
	return b, code, err
}

func ModelProbeMatches(payload []byte, code string) bool {
	var response struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(payload, &response) != nil {
		return false
	}
	for _, candidate := range response.Candidates {
		if candidate.FinishReason != "STOP" {
			continue
		}
		var text strings.Builder
		for _, part := range candidate.Content.Parts {
			if !part.Thought {
				text.WriteString(part.Text)
			}
		}
		normalized := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) || r == '`' {
				return -1
			}
			return unicode.ToLower(r)
		}, text.String())
		if normalized == strings.ToLower(code+"|red|blue|green") {
			return true
		}
	}
	return false
}
