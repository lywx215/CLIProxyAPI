package enterpriseweb

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestWebHistoryModeUsesAdmissionSnapshot(t *testing.T) {
	r := enabledRuntime()
	body := []byte(`{"contents":[{"parts":[{"text":"history"}]},{"parts":[{"inlineData":{"data":"YQ=="}}]}]}`)
	oldCtx, old, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, HistoryMode: config.EnterpriseWebSerializedContext})
	if code(ValidateHistoryMode(oldCtx, body)) != 409 {
		t.Fatal("old snapshot changed")
	}
	newCtx, next, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: false})
	if err := ValidateHistoryMode(newCtx, body); err != nil {
		t.Fatal("accepted request changed mode", err)
	}
	old.Release()
	next.Release()
	if r.Status()["active_requests"] != 0 {
		t.Fatal("leases leaked")
	}
}

func TestWebMediaClassificationDoesNotLoseOversizedHistory(t *testing.T) {
	body := []byte(`{"contents":[` + strings.Repeat(`{"role":"model","parts":[{"text":"fragment"}]},`, 4200) + `{"role":"user","parts":[{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}]}]}`)
	if !HasMedia(body) {
		t.Fatal("oversized media history fell back to text")
	}
	if !HasMedia([]byte(`{"contents":[{"parts":[{"fileData":{"fileUri":"old-id"}}]}]}`)) {
		t.Fatal("file reference missed validation")
	}
	if HasMedia([]byte(`{"contents":[{"parts":[{"text":"inlineData fileData"}]}]}`)) {
		t.Fatal("ordinary text rerouted")
	}
}

func TestWebPreflightRejectionsCountOnceAndExcludeSubmittedRequests(t *testing.T) {
	r := enabledRuntime()
	ctx, lease, _ := r.Acquire(context.Background(), 1024)
	err := InputFailure(400, "invalid_history", "contents", "Invalid history")
	r.RecordPreflight(ctx, err)
	r.RecordPreflight(ctx, err)
	lease.Release()
	ctx, lease, _ = r.Acquire(context.Background(), 1024)
	lease.MarkSessionStarted()
	r.RecordPreflight(ctx, err)
	lease.Release()
	if r.Status()["preflight_rejections"].(map[string]uint64)["invalid_history"] != 1 {
		t.Fatal(r.Status())
	}
}

func TestWebPreflightReadFailureReleasesAdmission(t *testing.T) {
	r := enabledRuntime()
	reader := io.MultiReader(strings.NewReader(`{"contents":[{"parts":[{"inlineData":`), iotest.ErrReader(io.ErrUnexpectedEOF))
	_, _, _, err := ReadBody(context.Background(), reader, -1, r)
	if err == nil || r.Status()["active_requests"] != 0 || r.Status()["inflight_bytes"] != int64(0) {
		t.Fatal("body failure was not released", err, r.Status())
	}
	if r.Status()["preflight_rejections"].(map[string]uint64)["unavailable"] != 1 {
		t.Fatal("read failure was not counted", r.Status())
	}
	ctx, lease, _ := r.Acquire(context.Background(), 1024)
	r.RecordPreflight(ctx, InputFailure(400, "untrusted-string", "", "Invalid"))
	lease.Release()
	if len(r.Status()["preflight_rejections"].(map[string]uint64)) != 1 {
		t.Fatal("unbounded diagnostic reason")
	}
}
