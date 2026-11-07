package enterpriseweb

import (
	"context"
	"hash/fnv"
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
	credentials          map[string]bool
	tokens               map[string]tokenState
	cookieChecks         map[string]cookieCheckState
	checkingCookie       bool
	models               map[string]map[string]bool
	credentialEpochs     map[string]uint64
	modelEpochs          map[string]uint64
	commitCookieRotation func(context.Context, string, uint64, Credential, Credential) (Credential, error)
	authLocks            [64]sync.Mutex
	rejectedConcurrency  uint64
	rejectedMemory       uint64
	preflightRejections  map[string]uint64
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
	once           sync.Once
	config         config.GeminiEnterpriseWebConfig
	rejectionOnce  sync.Once
	sessionStarted atomic.Bool
}

func NewRuntime() *Runtime {
	return &Runtime{settings: (config.GeminiEnterpriseWebConfig{}).WithDefaults(), credentials: map[string]bool{}, tokens: map[string]tokenState{}, cookieChecks: map[string]cookieCheckState{}, models: map[string]map[string]bool{}, credentialEpochs: map[string]uint64{}, modelEpochs: map[string]uint64{}, now: time.Now}
}

// SetCookieRotationCommitter connects the credential store before the runtime is used.
func (r *Runtime) SetCookieRotationCommitter(commit func(context.Context, string, uint64, Credential, Credential) (Credential, error)) {
	r.commitCookieRotation = commit
}
func (r *Runtime) Configure(c config.GeminiEnterpriseWebConfig) {
	c = c.WithDefaults()
	r.mu.Lock()
	r.settings = c
	r.enabled.Store(c.Enabled)
	r.mu.Unlock()
}
func (r *Runtime) Enabled() bool               { return r != nil && r.enabled.Load() }
func ExistingLease(ctx context.Context) *Lease { v, _ := ctx.Value(leaseKey{}).(*Lease); return v }
func WithBody(ctx context.Context, b []byte) context.Context {
	return context.WithValue(ctx, bodyKey{}, b)
}
func Body(ctx context.Context) ([]byte, bool) { v, ok := ctx.Value(bodyKey{}).([]byte); return v, ok }

// Reserve the complete processing envelope before reading the large media value.
func (r *Runtime) Acquire(ctx context.Context, bodyBytes int64) (context.Context, *Lease, error) {
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
		r.incrementPreflightLocked("channel_disabled")
		return ctx, nil, Fail(503, "Media input channel is disabled")
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
	l := &Lease{runtime: r, reserved: amount, config: settings}
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
		r.active--
		r.used -= l.reserved
		r.mu.Unlock()
	})
}
func (l *Lease) Budget() int64 {
	l.runtime.mu.Lock()
	defer l.runtime.mu.Unlock()
	return l.reserved
}
func (l *Lease) HistoryMode() string { return l.config.HistoryMode }
func (l *Lease) MarkSessionStarted() { l.sessionStarted.Store(true) }
func (r *Runtime) ClaimCredential(id string) (func(), error) {
	r.mu.Lock()
	if r.credentials[id] {
		r.rejectedConcurrency++
		r.mu.Unlock()
		return nil, Fail(429, "Selected media credential is busy")
	}
	r.credentials[id] = true
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { r.mu.Lock(); delete(r.credentials, id); r.mu.Unlock() }) }, nil
}
func (r *Runtime) Status() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	rejections := make(map[string]uint64, len(r.preflightRejections))
	for key, value := range r.preflightRejections {
		rejections[key] = value
	}
	return map[string]any{"enabled": r.settings.Enabled, "active_requests": r.active, "inflight_bytes": r.used, "max_concurrent": r.settings.MaxConcurrent, "max_inflight_bytes": r.settings.MaxInflightBytes, "draining": !r.settings.Enabled && r.active > 0, "rejected_concurrency": r.rejectedConcurrency, "rejected_memory": r.rejectedMemory, "history_mode": r.settings.HistoryMode, "supported_history_modes": []string{config.EnterpriseWebSingleTurn, config.EnterpriseWebSerializedContext}, "preflight_rejections": rejections, "input_limits": r.settings.InputLimits}
}
func (r *Runtime) AuthStatus(key string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.tokens[key]
	return map[string]any{"auth_verified": v.Token != "" && r.now().Before(v.Expires), "verified_at": v.Verified}
}
func (r *Runtime) authLock(key string) *sync.Mutex {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return &r.authLocks[h.Sum64()%64]
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
func (r *Runtime) ReplaceVerifiedCookies(id string, previous, next Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	check, ok := r.cookieChecks[cookieCheckKey(id, previous)]
	r.forgetCredentialLocked(id)
	if ok && check.status != "relogin_required" {
		r.cookieChecks[cookieCheckKey(id, next)] = check
	}
}

func (r *Runtime) forgetCredentialLocked(id string) {
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
	if check, ok := r.cookieChecks[cookieCheckKey(id, c)]; ok {
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
