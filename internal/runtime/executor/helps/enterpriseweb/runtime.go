package enterpriseweb

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

// Runtime is owned by one auth manager and survives executor/config replacement.
type Runtime struct {
	mu                   sync.Mutex
	settings             config.GeminiEnterpriseWebConfig
	active               int
	used                 int64
	cookieGates          map[string]*cookieGate
	tokens               map[string]tokenState
	cookieChecks         map[string]cookieCheckState
	checkingCookie       bool
	ccCommitMu           sync.RWMutex
	ccEpoch              uint64
	ccEnabledAt          time.Time
	ccDeferrals          uint64
	models               map[string]map[string]bool
	credentialEpochs     map[string]uint64
	modelEpochs          map[string]uint64
	cookieVersions       map[string]cookieVersionRange
	resolveCookie        func(CookieSelection, Credential) (Credential, error)
	commitCookieRotation func(context.Context, string, uint64, Credential, Credential) (Credential, error)
	rejectedConcurrency  uint64
	rejectedMemory       uint64
	preflightRejections  map[string]uint64
	fallbackDisabled     uint64
	fallbackNoCredential uint64
	enabled              atomic.Bool
	now                  func() time.Time
}
type tokenState struct {
	Token    string
	Expires  time.Time
	Verified time.Time
}
type leaseKey struct{}
type bodyKey struct{}
type Lease struct {
	runtime        *Runtime
	reserved       int64
	released       bool
	generation     bool
	once           sync.Once
	config         config.GeminiEnterpriseWebConfig
	rejectionOnce  sync.Once
	sessionStarted atomic.Bool
}

func NewRuntime() *Runtime {
	return &Runtime{
		settings:    (config.GeminiEnterpriseWebConfig{}).WithDefaults(),
		cookieGates: make(map[string]*cookieGate), tokens: make(map[string]tokenState),
		cookieChecks: make(map[string]cookieCheckState), models: make(map[string]map[string]bool),
		credentialEpochs: make(map[string]uint64), modelEpochs: make(map[string]uint64),
		cookieVersions: make(map[string]cookieVersionRange), now: time.Now,
	}
}

// SetCookieResolver connects the currently published credential snapshots before use.
func (r *Runtime) SetCookieResolver(resolve func(CookieSelection, Credential) (Credential, error)) {
	r.resolveCookie = resolve
}

// SetCookieRotationCommitter connects the credential store before the runtime is used.
func (r *Runtime) SetCookieRotationCommitter(commit func(context.Context, string, uint64, Credential, Credential) (Credential, error)) {
	r.commitCookieRotation = commit
}

// Configure reports whether the cookie scheduler must reconsider its deadlines.
func (r *Runtime) Configure(c config.GeminiEnterpriseWebConfig) bool {
	c = c.WithDefaults()
	r.ccCommitMu.Lock()
	defer r.ccCommitMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	webChanged := r.settings.Enabled != c.Enabled
	ccWasActive := r.cookieCCActiveLocked()
	r.settings = c
	r.enabled.Store(c.Enabled)
	ccChanged := ccWasActive != r.cookieCCActiveLocked()
	if ccChanged {
		r.ccEpoch++
		r.ccEnabledAt = r.now()
	}
	return webChanged || ccChanged
}
func (r *Runtime) Enabled() bool               { return r != nil && r.enabled.Load() }
func ExistingLease(ctx context.Context) *Lease { v, _ := ctx.Value(leaseKey{}).(*Lease); return v }
func WithBody(ctx context.Context, b []byte) context.Context {
	return context.WithValue(ctx, bodyKey{}, b)
}
func Body(ctx context.Context) ([]byte, bool) { v, ok := ctx.Value(bodyKey{}).([]byte); return v, ok }

// Reserve the complete processing envelope before reading the large media value.
func (r *Runtime) Acquire(ctx context.Context, bodyBytes int64, allowFallback ...bool) (context.Context, *Lease, error) {
	if old := ExistingLease(ctx); old != nil {
		return ctx, old, nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	settings := r.admissionConfig(ctx)
	maxBodyBytes := settings.InputLimits.MaxRequestBytes
	if bodyBytes < 0 {
		bodyBytes = maxBodyBytes
	}
	if bodyBytes > maxBodyBytes {
		err := InputFailureFrom(contract.LimitFailure("body", "Media request exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitRequestBytes, Observed: bodyBytes, Maximum: maxBodyBytes}))
		r.recordPreAdmissionFailure(ctx, "input_limit_exceeded", err)
		return ctx, nil, err
	}
	amount := 2*bodyBytes + bodyBytes*3/4 + (32 << 20)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.settings.Enabled {
		if len(allowFallback) == 0 || !allowFallback[0] {
			r.incrementPreflightLocked("channel_disabled")
		}
		return ctx, nil, newChannelDisabledError()
	}
	if r.active >= r.settings.MaxConcurrent {
		r.rejectedConcurrency++
		r.incrementPreflightLocked("capacity_exhausted")
		return ctx, nil, Fail(429, "Media concurrency capacity is exhausted")
	}
	if amount > r.settings.MaxInflightBytes-r.used {
		r.rejectedMemory++
		r.incrementPreflightLocked("capacity_exhausted")
		return ctx, nil, Fail(429, "Media buffer budget is exhausted")
	}
	l := &Lease{runtime: r, reserved: amount, config: settings, generation: true}
	r.active++
	r.used += amount
	return context.WithValue(ctx, leaseKey{}, l), l, nil
}
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		r := l.runtime
		r.mu.Lock()
		l.released = true
		if l.generation {
			r.active--
		}
		r.used -= l.reserved
		r.mu.Unlock()
	})
}
func (l *Lease) Budget() int64 {
	l.runtime.mu.Lock()
	defer l.runtime.mu.Unlock()
	return l.reserved
}
func (l *Lease) HistoryMode() string  { return l.config.HistoryMode }
func (l *Lease) MarkSessionStarted()  { l.sessionStarted.Store(true) }
func (l *Lease) SessionStarted() bool { return l != nil && l.sessionStarted.Load() }
func (r *Runtime) Status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	rejections := make(map[string]uint64, len(r.preflightRejections))
	for key, value := range r.preflightRejections {
		rejections[key] = value
	}
	return map[string]any{
		"enabled": r.settings.Enabled, "active_requests": r.active, "inflight_bytes": r.used,
		"cookie_cc_polling_enabled":  r.settings.CookieCCPollingEnabled,
		"cookie_cc_polling_deferred": r.ccDeferrals,
		"max_concurrent":             r.settings.MaxConcurrent, "max_inflight_bytes": r.settings.MaxInflightBytes,
		"draining": !r.settings.Enabled && r.active > 0, "rejected_concurrency": r.rejectedConcurrency, "rejected_memory": r.rejectedMemory,
		"history_mode": r.settings.HistoryMode, "supported_history_modes": []string{config.EnterpriseWebSingleTurn, config.EnterpriseWebSerializedContext},
		"history_count_policy": "unlimited", "supports_model_history_inline_data": true,
		"preflight_rejections": rejections, "input_limits": r.settings.InputLimits,
		"media_fallback_requests": map[string]uint64{
			string(FallbackWebDisabled): r.fallbackDisabled, string(FallbackNoCredential): r.fallbackNoCredential,
		},
	}
}
func (r *Runtime) AuthStatus(key string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.tokens[key]
	return map[string]any{"auth_verified": v.Token != "" && r.now().Before(v.Expires), "verified_at": v.Verified}
}

// RequestContext preserves admission and cancellation through the native HTTP handler.
func RequestContext(ctx context.Context) context.Context {
	if _, ok := Body(ctx); ok {
		return ctx
	}
	return context.Background()
}

// ForgetCredential invalidates cached authentication without disturbing admitted requests.
func (r *Runtime) ForgetCredential(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgetCredentialLocked(id)
}

// ReplaceVerifiedCookies invalidates authentication caches while retaining the
// scheduled rotation deadline across an already verified automatic Cookie update.
func (r *Runtime) ReplaceVerifiedCookies(id string, previous, next Credential, previousVersion, nextVersion uint64, preserveEvidence bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	check, ok := r.cookieChecks[cookieCheckKey(id, previous)]
	versions := r.cookieVersions[id]
	var evidence map[string]bool
	if preserveEvidence {
		evidence = r.models[modelEvidenceKey(id, previous)]
	}
	r.forgetCredentialLocked(id)
	if previousVersion != 0 && nextVersion == previousVersion+1 {
		if versions.latest != previousVersion || versions.first == 0 {
			versions.first = previousVersion
		}
		versions.latest = nextVersion
		r.cookieVersions[id] = versions
	}
	if ok && check.status != "relogin_required" {
		r.cookieChecks[cookieCheckKey(id, next)] = check
	}
	if len(evidence) != 0 {
		r.models[modelEvidenceKey(id, next)] = evidence
	}
}

func (r *Runtime) forgetCredentialLocked(id string) {
	delete(r.cookieVersions, id)
	r.credentialEpochs[id]++
	r.modelEpochs[id]++
	for key := range r.cookieChecks {
		if strings.HasPrefix(key, id+":") {
			delete(r.cookieChecks, key)
		}
	}
	for k := range r.tokens {
		if strings.HasPrefix(k, id+":") {
			delete(r.tokens, k)
		}
	}
	for k := range r.models {
		if strings.HasPrefix(k, id+":") {
			delete(r.models, k)
		}
	}
}
func (r *Runtime) CredentialStatus(id string, c Credential) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest tokenState
	for k, v := range r.tokens {
		if strings.HasPrefix(k, id+":"+c.Fingerprint()+":") && v.Verified.After(latest.Verified) {
			latest = v
		}
	}
	ids := []string{}
	for model := range r.models[modelEvidenceKey(id, c)] {
		ids = append(ids, model)
	}
	sort.Strings(ids)
	status := map[string]any{"auth_verified": latest.Token != "" && r.now().Before(latest.Expires), "verified_at": latest.Verified, "tested_model_ids": ids}
	check := r.cookieChecks[cookieCheckKey(id, c)]
	if check.status != "" {
		status["cookie_check_status"] = check.status
		status["cookie_checked_at"] = check.at
		if !check.verified.IsZero() {
			status["cookie_last_verified_at"] = check.verified
		}
		if r.settings.Enabled && !check.next.IsZero() {
			status["cookie_next_check_at"] = check.next
		}
		if check.reason != "" {
			status["cookie_check_reason"] = check.reason
		}
	} else if !latest.Verified.IsZero() {
		status["cookie_last_verified_at"] = latest.Verified
	}
	if r.settings.CookieCCPollingEnabled || !c.LastCCRenewedAt.IsZero() {
		status["cookie_cc_check"] = r.cookieCCStatusLocked(id, c, check)
	}
	return status
}

func (r *Runtime) CredentialEpoch(id string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.credentialEpochs[id]
}

func (r *Runtime) EvidenceEpoch(id string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.modelEpochs[id]
}

// Policy changes invalidate generation evidence without discarding authentication.
func (r *Runtime) ForgetModelEvidence(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modelEpochs[id]++
	delete(r.cookieVersions, id)
	for key := range r.models {
		if strings.HasPrefix(key, id+":") {
			delete(r.models, key)
		}
	}
}

func (r *Runtime) RecordModel(id string, metadata map[string]any, model string, epoch ...uint64) {
	c, err := CredentialFrom(metadata)
	if err != nil {
		return
	}
	r.RecordCredentialModel(id, c, model, epoch...)
}

func (r *Runtime) RecordCredentialModel(id string, c Credential, model string, epoch ...uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(epoch) > 0 && epoch[0] != r.modelEpochs[id] {
		return
	}
	key := modelEvidenceKey(id, c)
	if r.models[key] == nil {
		r.models[key] = map[string]bool{}
	}
	r.models[key][model] = true
}

func modelEvidenceKey(id string, c Credential) string {
	return id + ":" + c.Fingerprint() + ":" + c.Policy() + ":" + strconv.FormatUint(c.Revision, 10)
}
