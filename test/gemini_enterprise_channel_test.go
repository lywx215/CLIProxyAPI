package test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestEnterpriseNewAPIDefaultChannelTest(t *testing.T) {
	for _, model := range []string{"gemini-3.8-flash", "gemini-3.1-pro-preview"} {
		for _, protocol := range []string{"gemini", "openai"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model, protocol, stream), func(t *testing.T) {
					router, transport, _ := enterpriseFixtureRouter(t)
					fields := map[string]any{"generationConfig": map[string]int{"maxOutputTokens": 3000}, "contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": "hi"}}}}}
					if protocol == "openai" {
						fields = map[string]any{"max_tokens": 16, "stream_options": map[string]bool{"include_usage": true}}
					}
					path, body := enterprisePublicRequest(t, model, protocol, stream, fields)
					response := enterprisePublicCall(router, path, body)
					if response.Code != 200 || transport.calls.Load() != 1 || !strings.Contains(enterprisePublicText(response.Body.Bytes(), protocol, stream), "offline") {
						t.Fatalf("default channel test failed: status=%d body=%s", response.Code, response.Body.String())
					}
					query := gjson.GetBytes(transport.requestBody(), "query.text").String()
					if !strings.HasPrefix(query, "Continue the conversation below.") || !strings.Contains(query, `"role":"user","text":"hi"`) {
						t.Fatal("default test did not receive legacy greeting compatibility")
					}
					if stream && !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
						t.Fatal("streaming channel test lost SSE framing")
					}
					packets := []string{response.Body.String()}
					if stream {
						packets = nil
						for _, line := range strings.Split(response.Body.String(), "\n") {
							if strings.HasPrefix(line, "data:") {
								packets = append(packets, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
							}
						}
					}
					usagePackets := 0
					for _, packet := range packets {
						key := "usageMetadata"
						if protocol == "openai" {
							key = "usage"
						}
						if values := gjson.Get(packet, key); values.Exists() {
							usagePackets++
							if len(values.Map()) != 3 {
								t.Fatal("unknown token subfields became zero")
							}
						}
					}
					if usagePackets != 1 {
						t.Fatalf("usage packets=%d", usagePackets)
					}
				})
			}
		}
	}
}
