package enterpriseweb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

func TestWebAdmissionKeepsInputLimitSnapshotAndReleasesGrowth(t *testing.T) {
	r := enabledRuntime()
	cfg := config.GeminiEnterpriseWebConfig{Enabled: true, HistoryMode: config.EnterpriseWebSerializedContext, InputLimits: config.GeminiEnterpriseWebInputLimits{MaxTextBytes: 70}}
	r.Configure(cfg)
	ctx, first, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	cfg.InputLimits.MaxTextBytes = 10
	r.Configure(cfg)
	_, second, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if first.InputLimits().MaxTextBytes != 70 || second.InputLimits().MaxTextBytes != 10 {
		t.Fatal("in-flight limits changed")
	}
	if err := first.EnsureBudget(ctx, 80<<20); err != nil {
		t.Fatal(err)
	}
	if first.Budget() != 80<<20 {
		t.Fatal("growth was not reserved")
	}
	if err := first.EnsureBudget(ctx, 300<<20); code(err) != 429 {
		t.Fatal("global memory budget bypassed", err)
	}
	second.Release()
	first.Release()
	first.Release()
	if err := first.EnsureBudget(ctx, 90<<20); code(err) != 503 {
		t.Fatal("released lease grew", err)
	}
	if r.Status()["active_requests"] != 0 || r.Status()["inflight_bytes"] != int64(0) {
		t.Fatal("grown reservation leaked", r.Status())
	}
}

func TestWebConfiguredBodyLimitDoesNotLimitPlainEnterpriseRequests(t *testing.T) {
	r := enabledRuntime()
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, InputLimits: config.GeminiEnterpriseWebInputLimits{MaxRequestBytes: 128}})
	plain := `{"contents":[{"parts":[{"text":"` + strings.Repeat("x", 256) + `"}]}]}`
	_, _, lease, err := ReadBody(context.Background(), strings.NewReader(plain), int64(len(plain)), r)
	if err != nil || lease != nil {
		t.Fatal("plain text acquired Web limits", err)
	}
	media := `{"contents":[{"parts":[{"text":"` + strings.Repeat("x", 256) + `"},{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}]}]}`
	_, _, lease, err = ReadBody(context.Background(), strings.NewReader(media), int64(len(media)), r)
	var failure *Failure
	if lease != nil || !errors.As(err, &failure) || failure.Code != 413 || failure.Limit == nil || failure.Limit.Name != contract.LimitDeclaredRequestBytes || failure.Limit.Maximum != 128 {
		t.Fatal("media body limit was not applied", err)
	}
	if r.Status()["inflight_bytes"] != int64(0) {
		t.Fatal("oversize body leaked admission")
	}
}
