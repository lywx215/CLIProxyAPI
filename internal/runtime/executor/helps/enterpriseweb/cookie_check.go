package enterpriseweb

import (
	"errors"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	cookieCheckInterval = 10 * time.Minute
	cookieCheckBackoff  = 5 * time.Minute
)

type cookieCheckState struct {
	status   string
	at       time.Time
	next     time.Time
	verified time.Time
	reason   cookieCheckReason
	cc       cookieCCCheckState
}

type cookieCheckReason string

const (
	cookieRotationUnauthorized cookieCheckReason = "rotation_unauthorized"
	cookieCandidateRejected    cookieCheckReason = "candidate_rejected"
	cookieSessionRejected      cookieCheckReason = "session_rejected"
	cookieRateLimited          cookieCheckReason = "rate_limited"
	cookieAccessDenied         cookieCheckReason = "access_denied"
	cookieTemporaryFailure     cookieCheckReason = "temporary_failure"
)

type upstreamCookieFailure struct {
	error
	retry time.Duration
}

func (e *upstreamCookieFailure) Unwrap() error { return e.error }

func cookieHTTPFailure(err error, res *http.Response, now time.Time) error {
	raw := strings.TrimSpace(res.Header.Get("Retry-After"))
	if raw == "" {
		return err
	}
	var retry time.Duration
	if seconds, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil && seconds > 0 {
		retry = time.Duration(min(seconds, int64((1<<63-1)/time.Second))) * time.Second
	} else if at, parseErr := http.ParseTime(raw); parseErr == nil {
		retry = at.Sub(now)
	}
	if retry > 0 {
		return &upstreamCookieFailure{error: err, retry: retry}
	}
	return err
}

// Use a stable offset per credential to spread checks after a shared startup.
func cookieCheckOffset(id string) time.Duration {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	return time.Duration(hash.Sum32()%31) * time.Second
}

func cookieCheckKey(id string, c Credential) string { return id + ":" + c.Fingerprint() }

type loginRequiredError struct{ *Failure }

func loginRequired() error {
	const message = "Google requires a new login; sign in again and update the Web Cookie"
	return &loginRequiredError{&Failure{Code: http.StatusUnauthorized, Message: message, clientMessage: message}}
}

func (e *loginRequiredError) Unwrap() error { return e.Failure }

func isLoginRedirect(res *http.Response) bool {
	if res.StatusCode < 300 || res.StatusCode >= 400 {
		return false
	}
	target, err := res.Location()
	return err == nil && target.Scheme == "https" && target.Host == "accounts.google.com" && target.User == nil
}

// ClaimCookieCheck limits scheduled checks across accounts. Cookie writers wait
// for preparations through the same gate as foreground authentication.
func (r *Runtime) ClaimCookieCheck() func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.settings.Enabled || r.checkingCookie {
		return nil
	}
	r.checkingCookie = true
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.checkingCookie = false
	}
}

func (r *Runtime) recordCookieOutcome(id string, epoch uint64, c Credential, err error, scheduled bool, degraded cookieCheckReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.credentialEpochs[id] != epoch {
		return
	}
	key := cookieCheckKey(id, c)
	state := r.cookieChecks[key]
	previousStatus := state.status
	state.status, state.at, state.reason = "verified", r.now(), ""
	interval := cookieCheckInterval + cookieCheckOffset(id)
	var loginErr *loginRequiredError
	if errors.As(err, &loginErr) {
		r.markCookieReloginLocked(id, &state)
	} else if err != nil {
		state.status, state.reason, interval = "check_failed", cookieTemporaryFailure, cookieCheckBackoff
		switch failureStatus(err) {
		case http.StatusTooManyRequests:
			state.reason = cookieRateLimited
		case http.StatusForbidden:
			state.reason = cookieAccessDenied
		}
		var upstream *upstreamCookieFailure
		if errors.As(err, &upstream) {
			interval = max(interval, upstream.retry)
		}
		if previousStatus == "relogin_required" {
			state.status, state.reason = previousStatus, cookieSessionRejected
		}
	} else {
		state.verified = state.at
		if !scheduled && (previousStatus == "relogin_required" || state.cc.status == cookieCCUnsupported) {
			state.cc = cookieCCCheckState{epoch: r.ccEpoch, next: state.at.Add(cookieCCOffset(id))}
		}
		if degraded != "" {
			state.status, state.reason, interval = "renewal_degraded", degraded, cookieCheckBackoff
		}
	}
	if scheduled && state.status != "relogin_required" {
		state.next = state.at.Add(interval)
	}
	r.cookieChecks[key] = state
}

func (r *Runtime) markCookieReloginLocked(id string, state *cookieCheckState) {
	state.status, state.next, state.reason = "relogin_required", time.Time{}, cookieSessionRejected
	for key := range r.tokens {
		if strings.HasPrefix(key, id+":") {
			delete(r.tokens, key)
		}
	}
}

func (r *Runtime) CookieReloginRequired(id string, c Credential) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cookieChecks[cookieCheckKey(id, c)].status == "relogin_required"
}

// NextCookieCheck suspends rejected sessions until a credential change or a successful manual check.
func (r *Runtime) NextCookieCheck(id string, c Credential, now time.Time) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.settings.Enabled {
		return time.Time{}, false
	}
	check := r.cookieChecks[cookieCheckKey(id, c)]
	if check.status == "relogin_required" {
		return time.Time{}, false
	}
	if check.next.IsZero() {
		return now, true
	}
	if nextCC := r.cookieCCNextLocked(id, check); !nextCC.IsZero() && nextCC.Before(check.next) {
		return nextCC, true
	}
	return check.next, true
}
