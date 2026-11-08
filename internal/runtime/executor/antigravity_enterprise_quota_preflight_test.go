package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ae "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravityenterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityEnterpriseExpiredQuotaReadFailureNeverGeneratesOrCools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, metadataStatus := range []int{403, 503} {
			t.Run(fmt.Sprintf("stream=%v/status=%d", stream, metadataStatus), func(t *testing.T) {
				var reads, generated atomic.Int32
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", aeTransport(func(req *http.Request) (*http.Response, error) {
					if strings.HasSuffix(req.URL.Path, ":fetchQuotaStatus") {
						reads.Add(1)
						return aeResponse(metadataStatus, "application/json", fmt.Sprintf(`{"error":{"code":%d}}`, metadataStatus)), nil
					}
					generated.Add(1)
					return aeResponse(200, "application/json", aeAnswer), nil
				}))
				auth := aeExecutorAuth("global")
				e := NewAntigravityEnterpriseExecutor(&config.Config{})
				var expired ae.Quota
				if err := json.Unmarshal([]byte(aeQuotaPositive), &expired); err != nil {
					t.Fatal(err)
				}
				expired.ObservedAt = time.Now().Add(-ae.QuotaTTL)
				e.metadata[antigravityEnterpriseMetadataKey(auth)] = antigravityEnterpriseMetadata{
					config: ae.Config{}, configAt: time.Now(), quota: expired,
				}
				req := coreexec.Request{Model: "gemini-3.8-flash-low", Payload: aePayload(translator.FormatGemini)}
				opts := coreexec.Options{SourceFormat: translator.FormatGemini}
				var err error
				if stream {
					_, err = e.ExecuteStream(ctx, auth, req, opts)
				} else {
					_, err = e.Execute(ctx, auth, req, opts)
				}
				var failure *ae.Error
				if !errors.As(err, &failure) || failure.Code != "metadata_pending" || failure.HTTP != 503 || failure.IsGenerationAttempted() {
					t.Fatalf("metadata failure was misclassified: %v", err)
				}
				result := coreauth.AntigravityEnterpriseResultError(err)
				if result == nil || result.Code != "antigravity_enterprise_local_gate" || result.HTTPStatus != 503 {
					t.Fatalf("metadata failure can cool a valid credential: %+v", result)
				}
				if reads.Load() != 1 || generated.Load() != 0 {
					t.Fatalf("expired quota reached generation: reads=%d generation=%d", reads.Load(), generated.Load())
				}
			})
		}
	}
}
