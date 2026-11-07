package enterpriseweb

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestWebGrowingBudgetKeepsExistingReservationAfterLimitReduction(t *testing.T) {
	r := enabledRuntime()
	ctx, lease, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.EnsureBudget(ctx, 80<<20); err != nil {
		t.Fatal(err)
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: false, MaxInflightBytes: 64 << 20})
	if err := lease.EnsureBudget(ctx, 80<<20); err != nil {
		t.Fatal("existing reservation revoked", err)
	}
	if err := lease.EnsureBudget(ctx, 81<<20); err != nil {
		t.Fatal("limit reduction revoked an admitted request's original ceiling", err)
	}
	if err := lease.EnsureBudget(ctx, 257<<20); code(err) != 429 {
		t.Fatal("growth bypassed the admission ceiling", err)
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: false, MaxInflightBytes: 512 << 20})
	if err := lease.EnsureBudget(ctx, 257<<20); code(err) != 429 {
		t.Fatal("limit increase changed the accepted request's snapshot", err)
	}
	if r.Status()["inflight_bytes"] != int64(81<<20) || !r.Status()["draining"].(bool) {
		t.Fatal("failed growth changed active reservation", r.Status())
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, MaxInflightBytes: 64 << 20})
	if _, _, err := r.Acquire(context.Background(), 1024); code(err) != 429 {
		t.Fatal("new admission ignored the reduced limit after reopening", err)
	}
	lease.Release()
	lease.Release()
	if r.Status()["inflight_bytes"] != int64(0) || r.Status()["active_requests"] != 0 {
		t.Fatal("grown lease leaked on drain", r.Status())
	}
}

func TestWebGrowingBudgetDoesNotReserveAfterCancellation(t *testing.T) {
	r := enabledRuntime()
	ctx, lease, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	initial := lease.Budget()
	if err := lease.EnsureBudget(cancelled, initial+1024); err == nil || lease.Budget() != initial || r.Status()["inflight_bytes"] != initial {
		t.Fatal("cancelled request changed provider reservation", err, r.Status())
	}
}

func TestWebGrowingBudgetCountsOtherLeasesAcrossLimitChanges(t *testing.T) {
	r := enabledRuntime()
	ctx, first, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	_, second, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	initial := first.Budget()
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, MaxInflightBytes: 64 << 20})
	if err := first.EnsureBudget(ctx, 256<<20); code(err) != 429 || first.Budget() != initial {
		t.Fatal("growth ignored the other live lease", err, r.Status())
	}
	if err := first.EnsureBudget(ctx, (256<<20)-second.Budget()); err != nil {
		t.Fatal("growth rejected the original shared ceiling", err)
	}
	if r.Status()["inflight_bytes"] != int64(256<<20) {
		t.Fatal("growth was not charged to the runtime", r.Status())
	}
	second.Release()
	if err := first.EnsureBudget(ctx, 256<<20); err != nil {
		t.Fatal("released space could not be reused by the admitted request", err)
	}
	first.Release()
	if r.Status()["inflight_bytes"] != int64(0) {
		t.Fatal("final lease release leaked budget", r.Status())
	}
}
