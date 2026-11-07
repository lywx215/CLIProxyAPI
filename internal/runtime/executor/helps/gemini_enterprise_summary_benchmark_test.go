package helps

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

var enterpriseSummaryBenchmarkResult []byte

// Measure the actual helper separately from HTTP benchmarks whose executor is
// an immediate Mock. These cases perform no authentication or network calls.
func BenchmarkEnterpriseSummaryMapping(b *testing.B) {
	for _, model := range []string{"gemini-4-argon", "gemini-3.7-flash", "gemini-3.8-flash", "gemini-4-carbon"} {
		modes := []string{"absent"}
		if model == "gemini-4-carbon" {
			modes = []string{"absent", "true", "false"}
		}
		for _, mode := range modes {
			for _, size := range []int{1024, 32768} {
				b.Run(fmt.Sprintf("%s/%s/%dKiB", model, mode, size/1024), func(b *testing.B) {
					text := strings.Repeat("x", size)
					source := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}}}
					if mode != "absent" {
						source["generationConfig"] = map[string]any{"thinkingConfig": map[string]any{"includeThoughts": mode == "true"}}
					}
					payload, err := json.Marshal(source)
					if err != nil {
						b.Fatal(err)
					}
					body, err := json.Marshal(map[string]any{"query": map[string]any{"text": text}, "generationSpec": map[string]any{"modelId": model}})
					if err != nil {
						b.Fatal(err)
					}
					req := coreexec.Request{Model: model, Payload: payload, Format: translator.FormatGemini}
					opts := coreexec.Options{SourceFormat: translator.FormatGemini}
					b.ReportAllocs()
					b.SetBytes(int64(size))
					b.ResetTimer()
					for range b.N {
						result, err := ApplyEnterpriseThinking(body, req, opts)
						if err != nil {
							b.Fatal(err)
						}
						enterpriseSummaryBenchmarkResult = result
					}
					b.StopTimer()
					enterpriseSummaryBenchmarkResult = nil
				})
			}
		}
	}
}
