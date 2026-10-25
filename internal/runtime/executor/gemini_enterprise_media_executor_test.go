package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestEnterpriseMediaAllFormatsAndStreams(t *testing.T) {
	var pngData bytes.Buffer
	_ = png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	for _, kind := range []string{"image", "video"} {
		for _, protocol := range []string{"openai", "gemini", "openai-" + kind} {
			for _, stream := range []bool{false, true} {
				if stream && protocol == "openai-"+kind {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%t", kind, protocol, stream), func(t *testing.T) {
					auth := enterpriseExecutorAuth()
					engine := ge.Engine(ge.FromMetadata(auth.Metadata))
					mime := "image/png"
					data := pngData.Bytes()
					if kind == "video" {
						mime = "video/mp4"
						data = []byte("\x00\x00\x00\x18ftypisomtest")
					}
					generated, downloads := 0, 0
					transport := enterpriseTransport(func(r *http.Request) (*http.Response, error) {
						if r.Header.Get("Authorization") != "Bearer access" {
							t.Fatal("download changed account")
						}
						if r.Method == http.MethodPost {
							generated++
							body, _ := io.ReadAll(r.Body)
							if gjson.GetBytes(body, "generationSpec").Exists() || gjson.GetBytes(body, "query.text").String() != "configured" || !gjson.GetBytes(body, "toolsSpec."+kind+"GenerationSpec").Exists() {
								t.Fatal("payload barrier was bypassed")
							}
							reply := fmt.Sprintf(`[{"sessionInfo":{"session":%q},"assistToken":"secret","answer":{"state":"IN_PROGRESS","replies":[{"content":{"file":{"fileId":"file-1","mimeType":%q}}},{"content":{"file":{"fileId":"file-1","mimeType":%q}}}]}},{"answer":{"state":"SUCCEEDED"}}]`, engine+"/sessions/123", mime, mime)
							return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(reply))}, nil
						}
						downloads++
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(base64.StdEncoding.EncodeToString(data)))}, nil
					})
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
					model := "gemini-3-pro-image"
					if kind == "video" {
						model = "veo-3.1"
					}
					cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: "gemini-enterprise"}}, Params: map[string]any{"query.text": "configured"}}}}}
					e := NewGeminiEnterpriseExecutor(cfg)
					body := []byte(`{"messages":[{"role":"user","content":"cat"}]}`)
					if stream && protocol == "openai" {
						body = []byte(`{"messages":[{"role":"user","content":"cat"}],"stream_options":{"include_usage":true}}`)
					}
					op := "chat_completions"
					if protocol == "gemini" {
						body = []byte(`{"contents":[{"parts":[{"text":"cat"}]}]}`)
						op = "generate_content"
					}
					if protocol == "openai-"+kind {
						body = []byte(`{"prompt":"cat"}`)
						op = kind + "_generation"
					}
					opts := coreexec.Options{SourceFormat: translator.FromString(protocol), Stream: stream, Metadata: map[string]any{coreexec.OperationMetadataKey: op}}
					req := coreexec.Request{Model: model, Payload: body}
					var result []byte
					if stream {
						out, err := e.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range out.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							result = append(result, chunk.Payload...)
						}
					} else {
						out, err := e.Execute(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						result = out.Payload
					}
					if generated != 1 || downloads != 1 {
						t.Fatalf("generation=%d downloads=%d", generated, downloads)
					}
					if !bytes.Contains(result, []byte(base64.StdEncoding.EncodeToString(data))) || bytes.Contains(result, []byte("secret")) || bytes.Contains(result, []byte("file-1")) {
						t.Fatal("lost media or leaked metadata")
					}
					if stream && protocol == "openai" && !bytes.Contains(result, []byte(`"finish_reason":"stop"`)) {
						t.Fatal("missing terminal packet")
					}
					if stream && protocol == "openai" && (!bytes.Contains(result, []byte(`"choices":[]`)) || !bytes.Contains(result, []byte(`"usage"`))) {
						t.Fatal("missing requested usage tail")
					}
				})
			}
		}
	}
}

func TestEnterpriseMediaFilteredToolAndDownloadFailure(t *testing.T) {
	for _, filter := range []bool{true, false} {
		cfg := &config.Config{}
		if filter {
			cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gemini-3-pro-image", Protocol: "gemini-enterprise"}}, Params: []string{"toolsSpec.imageGenerationSpec"}}}
		}
		calls := 0
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", enterpriseTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[{"answer":{"state":"IN_PROGRESS"}},{"answer":{"state":"SUCCEEDED"}}]`))}, nil
		}))
		_, err := NewGeminiEnterpriseExecutor(cfg).Execute(ctx, enterpriseExecutorAuth(), coreexec.Request{Model: "gemini-3-pro-image", Payload: []byte(`{"messages":[{"role":"user","content":"cat"}]}`)}, enterpriseOptions(translator.FormatOpenAI))
		if err == nil || !err.(interface{ IsRequestScoped() bool }).IsRequestScoped() {
			t.Fatal("failure would allow regeneration")
		}
		want := 1
		if filter {
			want = 0
		}
		if calls != want {
			t.Fatal("repaired filtered tool or regenerated")
		}
	}
}
