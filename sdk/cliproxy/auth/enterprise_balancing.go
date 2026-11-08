package auth

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Observations expire independently of their usage watermark: an idle pool can
// have a recent successful read whose last usage point is several hours old.
const EnterpriseQuotaObservationTTL = 5 * time.Minute

func enterpriseQuotaDayStart(now time.Time) time.Time {
	zone, _ := time.LoadLocation("America/Los_Angeles")
	local := now.In(zone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone).UTC()
}

type enterpriseQuotaDebit struct {
	end   time.Time
	count int64
}

type enterpriseBalancePool struct {
	redirect *enterpriseBalancePool
	snapshot ge.QuotaSnapshot
	pool     *ge.QuotaPool
	inFlight int64
	byAuth   map[string]int64
	debits   []enterpriseQuotaDebit
}

func (p *enterpriseBalancePool) current() *enterpriseBalancePool {
	for p != nil && p.redirect != nil {
		p = p.redirect
	}
	return p
}

// Identity discovery must not hide requests admitted before the edition was
// known. Existing tickets follow the forwarding pointer when they complete.
func (c *enterpriseQuotaCoordinator) migrateBalanceLocked(old, next *enterpriseQuotaGate) {
	if old == nil || old.fingerprint != next.fingerprint || old.epoch != next.epoch || old.credential != next.credential || old.identity.ValidFor(ge.FromMetadata(old.policy.Metadata)) || !next.identity.ValidFor(ge.FromMetadata(next.policy.Metadata)) {
		return
	}
	previous := c.balances[old.balanceKey()].current()
	if previous == nil {
		return
	}
	current := c.balancePoolLocked(next)
	if previous == current {
		return
	}
	current.inFlight += previous.inFlight
	for id, count := range previous.byAuth {
		current.byAuth[id] += count
	}
	current.debits = append(current.debits, previous.debits...)
	if current.pool != nil {
		current.pruneDebits(*current.pool.UsageUpdatedAt)
	}
	previous.redirect = current
	delete(c.balances, old.balanceKey())
}

// EnterpriseQuotaBalanceView contains only routing estimates, never OAuth data.
type EnterpriseQuotaBalanceView struct {
	Enabled            bool       `json:"enabled"`
	Status             string     `json:"status"`
	PoolID             string     `json:"pool_id,omitempty"`
	ObservedAt         *time.Time `json:"observed_at,omitempty"`
	ResetAt            *time.Time `json:"reset_at,omitempty"`
	Remaining          *int64     `json:"remaining,omitempty"`
	EffectiveRemaining *int64     `json:"effective_remaining,omitempty"`
	InFlight           int64      `json:"in_flight"`
	CredentialInFlight int64      `json:"credential_in_flight"`
	LocalUnreported    int64      `json:"local_unreported"`
	Reserve            int64      `json:"reserve"`
}

func (m *Manager) EnterpriseQuotaBalance(auth *Auth) EnterpriseQuotaBalanceView {
	view := EnterpriseQuotaBalanceView{Enabled: m.EnterpriseQuotaBalancingEnabled(), Status: "unknown", Reserve: m.enterpriseQuotaReserve()}
	if auth == nil || auth.enterpriseQuota == nil {
		return view
	}
	c := &m.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.bindings[auth.ID]
	if g == nil {
		return view
	}
	key := g.balanceKey()
	view.PoolID = quotaDigest(key)
	p := c.balances[key]
	if p == nil {
		return view
	}
	view.InFlight, view.CredentialInFlight = p.inFlight, p.byAuth[auth.ID]
	for _, debit := range p.debits {
		view.LocalUnreported += debit.count
	}
	if p.pool != nil {
		observed, reset, remaining := p.snapshot.ObservedAt, *p.snapshot.ResetAt, *p.pool.Remaining
		view.ObservedAt, view.ResetAt, view.Remaining = &observed, &reset, &remaining
		view.Status = "stale"
	}
	if left, known := p.remaining(c.now(), view.Reserve); known {
		view.Status, view.EffectiveRemaining = "ok", &left
		if left == 0 {
			view.Status = "exhausted"
		}
	}
	return view
}

func (m *Manager) EnterpriseQuotaBalancingEnabled() bool {
	if m == nil || m.HomeEnabled() {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	return cfg != nil && cfg.Routing.GeminiEnterpriseQuotaBalancing
}

func (m *Manager) enterpriseQuotaReserve() int64 {
	if m == nil {
		return 0
	}
	cfg, _ := m.runtimeConfig.Load().(*config.Config)
	if cfg != nil {
		return max(0, cfg.Routing.GeminiEnterpriseQuotaReserve)
	}
	return 0
}

func (g *enterpriseQuotaGate) balanceKey() string {
	keys := g.keys()
	if key := keys["edition"]; key != "" {
		return key
	}
	return keys["credential"]
}

func (c *enterpriseQuotaCoordinator) balancePoolLocked(g *enterpriseQuotaGate) *enterpriseBalancePool {
	if c.balances == nil {
		c.balances = make(map[string]*enterpriseBalancePool)
	}
	key := g.balanceKey()
	if p := c.balances[key]; p != nil {
		return p
	}
	if len(c.balances) >= 256 {
		active := make(map[string]bool, len(c.bindings))
		for _, binding := range c.bindings {
			active[binding.balanceKey()] = true
		}
		for key, pool := range c.balances {
			if !active[key] && pool.current().inFlight == 0 {
				delete(c.balances, key)
			}
		}
	}
	p := &enterpriseBalancePool{byAuth: make(map[string]int64)}
	c.balances[key] = p
	return p
}

// ObserveEnterpriseQuota accepts only a current credential's verified edition.
// A failed, stale, foreign or out-of-order read cannot replenish a routing pool.
// Restrictions remain separate: a positive balance never clears a Google 429.
func (m *Manager) ObserveEnterpriseQuota(source *Auth, snapshot ge.QuotaSnapshot) bool {
	if source == nil || source.enterpriseQuota == nil || !m.EnterpriseQuotaBalancingEnabled() {
		return false
	}
	c := &m.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.bindings[source.ID]
	old := source.enterpriseQuota
	if g == nil || g.epoch != old.epoch || g.credential != old.credential || g.fingerprint != old.fingerprint || g.policy.ProxyURL != old.policy.ProxyURL || g.policy.Disabled || g.policy.Status == StatusDisabled {
		return false
	}
	binding := ge.FromMetadata(g.policy.Metadata)
	if !g.identity.ValidFor(binding) || snapshot.ProjectID != binding.ProjectID || snapshot.Location != binding.Location {
		return false
	}
	now := c.now()
	pool := ge.SelectQuotaPool(snapshot, g.identity, binding)
	if pool == nil || pool.Status != "ok" || pool.Remaining == nil || pool.Limit == nil || pool.Used == nil || pool.UsageUpdatedAt == nil || snapshot.PeriodStart == nil || snapshot.ResetAt == nil ||
		snapshot.ObservedAt.After(now) || now.Sub(snapshot.ObservedAt) > EnterpriseQuotaObservationTTL || !snapshot.PeriodStart.Equal(enterpriseQuotaDayStart(now)) || !snapshot.ResetAt.Equal(ge.NextPacificMidnight(now)) ||
		pool.UsageUpdatedAt.Before(*snapshot.PeriodStart) || pool.UsageUpdatedAt.After(snapshot.ObservedAt) || *pool.Remaining < 0 || *pool.Limit < 0 || *pool.Used < 0 {
		return false
	}
	p := c.balancePoolLocked(g)
	if p.snapshot.ObservedAt.After(snapshot.ObservedAt) {
		return false
	}
	if p.pool != nil && p.snapshot.PeriodStart != nil && p.snapshot.PeriodStart.Equal(*snapshot.PeriodStart) && p.pool.UsageUpdatedAt.After(*pool.UsageUpdatedAt) {
		return false
	}
	p.snapshot = ge.CloneQuotaSnapshot(snapshot)
	p.pool = ge.SelectQuotaPool(p.snapshot, g.identity, binding)
	// Recompute rather than trusting a caller-supplied derived remaining value.
	remaining := max(int64(0), *p.pool.Limit-*p.pool.Used)
	p.pool.Remaining = &remaining
	p.pruneDebits(*pool.UsageUpdatedAt)
	return true
}

func (p *enterpriseBalancePool) pruneDebits(watermark time.Time) {
	kept := p.debits[:0]
	for _, debit := range p.debits {
		if debit.end.After(watermark) {
			kept = append(kept, debit)
		}
	}
	p.debits = kept
}

func (p *enterpriseBalancePool) remaining(now time.Time, reserve int64) (int64, bool) {
	if p == nil || p.pool == nil || p.snapshot.ResetAt == nil || !now.Before(*p.snapshot.ResetAt) {
		return 0, false
	}
	if now.Sub(p.snapshot.ObservedAt) >= EnterpriseQuotaObservationTTL && *p.pool.Remaining > reserve {
		return 0, false
	}
	// A previously verified exhausted daily pool does not gain capacity merely
	// because a subsequent Monitoring read failed. A newer reading or reset can
	// replenish it; positive stale balances remain unknown.
	left := max(int64(0), *p.pool.Remaining-reserve)
	left = max(0, left-p.inFlight)
	for _, debit := range p.debits {
		left = max(0, left-debit.count)
	}
	return max(0, left), true
}

func (g *enterpriseQuotaGate) balanceBlockedLocked(now time.Time) (bool, time.Time) {
	if !g.manager.EnterpriseQuotaBalancingEnabled() || !g.enabled() {
		return false, time.Time{}
	}
	p := g.manager.enterpriseQuota.balances[g.balanceKey()]
	left, known := p.remaining(now, g.manager.enterpriseQuotaReserve())
	if !known || left > 0 {
		return false, time.Time{}
	}
	// Re-evaluate after observation expiry or daily reset; no permanent restriction
	// is inferred from delayed Monitoring data or a local reservation.
	retry := *p.snapshot.ResetAt
	if expires := p.snapshot.ObservedAt.Add(EnterpriseQuotaObservationTTL); expires.After(now) && expires.Before(retry) {
		retry = expires
	}
	return true, retry
}

func (t *enterpriseQuotaTicket) finishBalanceLocked(now time.Time, err error) {
	p := t.balance.current()
	if p == nil {
		return
	}
	p.inFlight--
	p.byAuth[t.gate.authID]--
	if p.byAuth[t.gate.authID] == 0 {
		delete(p.byAuth, t.gate.authID)
	}
	var upstream *ge.GenerationError
	started := errors.As(err, &upstream) && upstream.Started
	// Cancellation or a failed stream may already have consumed a generation.
	// Explicit pre-generation rejections refund the reservation.
	if err != nil && !started && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return
	}
	p.pruneDebits(enterpriseQuotaDayStart(now))
	end := now.Truncate(time.Minute).Add(time.Minute)
	if n := len(p.debits); n > 0 && p.debits[n-1].end.Equal(end) {
		p.debits[n-1].count++
	} else {
		p.debits = append(p.debits, enterpriseQuotaDebit{end: end, count: 1})
	}
}

// pickBalancedKey uses normalized smooth weights and caps one member at 80%
// when alternatives exist. This avoids long monopolies at extreme quota ratios.
func pickBalancedKey(credit, weights map[string]float64) string {
	keys := make([]string, 0, len(weights))
	for key := range weights {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	maximum := keys[0]
	for _, key := range keys {
		if weights[key] > weights[maximum] {
			maximum = key
		}
	}
	others := 0.0
	for _, key := range keys {
		if key != maximum {
			others += weights[key]
		}
	}
	if len(keys) > 1 && weights[maximum] > 4*others {
		weights[maximum] = 4 * others
	}
	total := others + weights[maximum]
	for key := range credit {
		if _, present := weights[key]; !present {
			delete(credit, key)
		}
	}
	chosen := keys[0]
	for _, key := range keys {
		credit[key] += weights[key] / total
		if credit[key] > credit[chosen] {
			chosen = key
		}
	}
	credit[chosen]--
	return chosen
}

// QuotaBalancedSelector preserves model eligibility and priority filtering. It
// compares shared pools once, so duplicate credentials cannot multiply capacity.
// The configured selector still decides between Enterprise and other providers.
type QuotaBalancedSelector struct {
	Fallback Selector
	rotation RoundRobinSelector
	mu       sync.Mutex
	credits  map[string]map[string]float64
}

func (s *QuotaBalancedSelector) creditFor(key string) map[string]float64 {
	if s.credits == nil || (s.credits[key] == nil && len(s.credits) >= 4096) {
		s.credits = make(map[string]map[string]float64)
	}
	if s.credits[key] == nil {
		s.credits[key] = make(map[string]float64)
	}
	return s.credits[key]
}

func (s *QuotaBalancedSelector) Pick(ctx context.Context, provider, model string, opts exec.Options, auths []*Auth) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	candidates := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if a != nil && a.Provider == ge.Provider && a.enterpriseQuota != nil && a.enterpriseQuota.manager.EnterpriseQuotaBalancingEnabled() && authWeight(a) <= 0 {
			continue
		}
		candidates = append(candidates, a)
	}
	available, err := getSelectorAvailableAuths(ctx, candidates, provider, model, time.Now())
	if err != nil {
		return nil, err
	}
	fallback := s.Fallback
	if fallback == nil {
		fallback = &s.rotation
	}
	enterprise := make([]*Auth, 0, len(available))
	for _, a := range available {
		if a.Provider == ge.Provider && a.enterpriseQuota != nil {
			enterprise = append(enterprise, a)
		}
	}
	if len(enterprise) == 0 {
		return fallback.Pick(ctx, provider, model, opts, available)
	}
	if len(enterprise) != len(available) {
		picked, errPick := fallback.Pick(ctx, provider, model, opts, available)
		if errPick != nil || picked == nil || picked.Provider != ge.Provider {
			return picked, errPick
		}
	}
	m := enterprise[0].enterpriseQuota.manager
	if !m.EnterpriseQuotaBalancingEnabled() {
		return fallback.Pick(ctx, provider, model, opts, available)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &m.enterpriseQuota
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	type candidatePool struct {
		key   string
		pool  *enterpriseBalancePool
		auths []*Auth
		left  int64
		known bool
	}
	pools := map[string]*candidatePool{}
	knownBalances := []int64{}
	for _, a := range enterprise {
		g := c.bindings[a.ID]
		if g == nil || g.manager != m || authWeight(a) <= 0 {
			continue
		}
		key := g.balanceKey()
		p := pools[key]
		if p == nil {
			pool := c.balancePoolLocked(g)
			left, known := pool.remaining(now, m.enterpriseQuotaReserve())
			if known && left == 0 && g.enabled() {
				continue
			}
			p = &candidatePool{key: key, pool: pool, left: left, known: known}
			pools[key] = p
			if known && left > 0 {
				knownBalances = append(knownBalances, left)
			}
		}
		p.auths = append(p.auths, a)
	}
	// Unknown pools receive the smallest known positive capacity, never an
	// unlimited balance. With no observations, this is least-in-flight routing.
	unknownCapacity := int64(1)
	if len(knownBalances) > 0 {
		sort.Slice(knownBalances, func(i, j int) bool { return knownBalances[i] < knownBalances[j] })
		unknownCapacity = knownBalances[0]
	}
	weights := make(map[string]float64, len(pools))
	for key, p := range pools {
		left := p.left
		if !p.known || left == 0 {
			left = unknownCapacity
		}
		score := float64(left) / float64(p.pool.inFlight+1)
		weights[key] = score
	}
	if len(weights) == 0 {
		return nil, newModelCooldownError(model, provider, time.Second)
	}
	// Normalize to request units so changing balances do not leave credits in
	// obsolete quota units. Smooth weighting spreads even sequential requests.
	stateKey := "pools:" + provider + ":" + canonicalModelKey(model)
	chosen := pickBalancedKey(s.creditFor(stateKey), weights)
	p := pools[chosen]
	credentialWeights := make(map[string]float64, len(p.auths))
	byID := make(map[string]*Auth, len(p.auths))
	for _, a := range p.auths {
		credentialWeights[a.ID] = float64(authWeight(a)) / float64(p.pool.byAuth[a.ID]+1)
		byID[a.ID] = a
	}
	credential := pickBalancedKey(s.creditFor("credentials:"+chosen+":"+canonicalModelKey(model)), credentialWeights)
	return byID[credential], nil
}

// Hold a cancellable selection turn until the executor reserves its pool. This
// closes the gap where concurrent selectors could all observe zero in-flight.
func (m *Manager) awaitEnterpriseBalanceTurn(ctx context.Context, providers []string) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	if !m.EnterpriseQuotaBalancingEnabled() {
		return ctx, nil
	}
	found := false
	for _, provider := range providers {
		found = found || provider == ge.Provider
	}
	if !found {
		return ctx, nil
	}
	c := &m.enterpriseQuota
	c.mu.Lock()
	if c.balanceTurn == nil {
		c.balanceTurn = make(chan struct{}, 1)
	}
	turn := c.balanceTurn
	c.mu.Unlock()
	select {
	case turn <- struct{}{}:
		return budget.WithAdmissionTurn(ctx, func() { <-turn; budget.ReleaseAdmissionTurn(ctx) }), nil
	case <-ctx.Done():
		return ctx, ctx.Err()
	}
}
