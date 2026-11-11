package enterpriseweb

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"sync"
	"time"
)

const (
	widgetCCAction      = "widgetAdvancedCompleteQuery"
	cookieCCInterval    = time.Minute
	cookieCCAuthTimeout = 10 * time.Second
	cookieCCUnsupported = "unsupported"
)

type cookieCCCheckState struct {
	epoch    uint64
	status   string
	at, next time.Time
	reason   cookieCheckReason
}

type cookieCCCommitKey struct{}

func cookieCCOffset(id string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return time.Duration(h.Sum32()%60) * time.Second
}

func (r *Runtime) cookieCCActiveLocked() bool {
	return r.settings.Enabled && r.settings.CookieCCPollingEnabled
}

func (r *Runtime) cookieCCNextLocked(id string, check cookieCheckState) time.Time {
	if !r.cookieCCActiveLocked() || check.status == "relogin_required" {
		return time.Time{}
	}
	if check.cc.epoch == r.ccEpoch {
		return check.cc.next
	}
	start := r.ccEnabledAt
	if check.at.After(start) {
		start = check.at
	}
	return start.Add(cookieCCOffset(id))
}

// The existing PSIDTS check owns the startup check and wins ties.
func (r *Runtime) dueCookieCCEpoch(id string, c Credential) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	check := r.cookieChecks[cookieCheckKey(id, c)]
	if r.cookieCCDueLocked(id, check, r.now()) {
		return r.ccEpoch
	}
	return 0
}

func (r *Runtime) cookieCCDueLocked(id string, check cookieCheckState, now time.Time) bool {
	next := r.cookieCCNextLocked(id, check)
	return check.next.After(now) && !next.IsZero() && !next.After(now)
}

// DeferCookieCC moves a due poll off the busy slot without retrying upstream or
// shifting the PSIDTS deadline. A short offset avoids recurring minute collisions.
func (r *Runtime) DeferCookieCC(id string, c Credential) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := cookieCheckKey(id, c)
	check := r.cookieChecks[key]
	now := r.now()
	if !r.cookieCCDueLocked(id, check, now) {
		return false
	}
	if check.cc.epoch != r.ccEpoch {
		check.cc = cookieCCCheckState{epoch: r.ccEpoch}
	}
	check.cc.next = now.Add(time.Second + cookieCCOffset(id)%(4*time.Second))
	r.cookieChecks[key] = check
	r.ccDeferrals++
	return true
}

// LockCookieCCCommit fences the store transaction against hot shutdown. The
// manager releases it as soon as the saved credential is published.
func (r *Runtime) LockCookieCCCommit(ctx context.Context) (func(), error) {
	epoch, ok := ctx.Value(cookieCCCommitKey{}).(uint64)
	if !ok {
		return nil, nil
	}
	r.ccCommitMu.RLock()
	r.mu.Lock()
	valid := r.cookieCCActiveLocked() && epoch == r.ccEpoch
	r.mu.Unlock()
	if !valid {
		r.ccCommitMu.RUnlock()
		return nil, staleCookieCC()
	}
	return sync.OnceFunc(r.ccCommitMu.RUnlock), nil
}

func staleCookieCC() error {
	return Fail(http.StatusConflict, "Web credential or CC polling settings changed")
}

func (r *Runtime) cookieCCCurrent(id string, configEpoch, credentialEpoch uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cookieCCActiveLocked() && r.ccEpoch == configEpoch && r.credentialEpochs[id] == credentialEpoch
}

func (r *Runtime) checkCookieCC(ctx context.Context, credential *Credential, base *http.Client, selection CookieSelection, route string, configEpoch uint64) (resultErr error) {
	if selection.Disabled || ctx.Err() != nil {
		return staleCookieCC()
	}
	gate, releaseGate := r.retainCookieGate(credential.CookieKey())
	defer releaseGate()
	if !gate.auth.TryAcquire(1) {
		r.DeferCookieCC(selection.ID, *credential)
		return nil
	}
	defer gate.auth.Release(1)
	if !gate.preparations.TryAcquire(allCookiePreparations) {
		r.DeferCookieCC(selection.ID, *credential)
		return nil
	}
	defer gate.preparations.Release(allCookiePreparations)
	if r.resolveCookie != nil && selection.RegistrationEpoch != 0 {
		current, err := r.resolveCookie(selection, *credential)
		if err != nil {
			return err
		}
		*credential = current
	}
	id, original := selection.ID, *credential
	if r.dueCookieCCEpoch(id, original) != configEpoch {
		return nil
	}
	r.mu.Lock()
	started, epoch := r.now(), r.credentialEpochs[id]
	cached := r.tokens[cookieTokenKey(id, original, route)]
	r.mu.Unlock()
	if !r.cookieCCCurrent(id, configEpoch, epoch) {
		return staleCookieCC()
	}

	status := "no_change"
	var reason cookieCheckReason
	var verifiedToken string
	defer func() {
		if ctx.Err() == nil && failureStatus(resultErr) != http.StatusConflict {
			r.recordCookieCCOutcome(id, *credential, route, configEpoch, epoch, started, status, reason, verifiedToken, resultErr)
		}
	}()
	authCtx, cancel := context.WithTimeout(ctx, cookieCCAuthTimeout)
	defer cancel()
	verify := func(c Credential) (string, error) {
		if !r.cookieCCCurrent(id, configEpoch, epoch) {
			return "", staleCookieCC()
		}
		access, _, err := verifyPlus(authCtx, base, c, r.now(), false)
		if err != nil {
			return "", err
		}
		encoded, _ := json.Marshal(access)
		return string(encoded), nil
	}
	verifySaved := func(why cookieCheckReason) error {
		status, reason = "retrying", why
		var err error
		verifiedToken, err = verify(original)
		return err
	}
	token := cached.Token
	if token == "" || !started.Before(cached.Expires) {
		var err error
		token, err = verify(original)
		if err != nil {
			return err
		}
		verifiedToken = token
	}
	if !r.cookieCCCurrent(id, configEpoch, epoch) {
		return staleCookieCC()
	}
	if r.commitCookieRotation == nil {
		return Fail(http.StatusServiceUnavailable, "Web Cookie store is unavailable")
	}
	started = r.now()
	// Google requires a non-query suggestion type when the query is empty.
	response, err := postJSONAt(authCtx, base, original, token, widgetCCAction, map[string]any{
		"configId":         original.ConfigID,
		"additionalParams": map[string]string{"token": "-", "origin": "ORIGIN_UNSPECIFIED"},
		"advancedCompleteQueryRequest": map[string]any{
			"query":           "",
			"suggestionTypes": []string{"CONTENT"},
		},
	}, started)
	if err != nil {
		switch failureStatus(err) {
		case http.StatusUnauthorized:
			return verifySaved(cookieRotationUnauthorized)
		case http.StatusForbidden:
			return verifySaved(cookieAccessDenied)
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
			status, reason = cookieCCUnsupported, "endpoint_unsupported"
			return nil
		default:
			return err
		}
	}
	candidate := mergeResponseCookies(original, response, r.now(), "SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC")
	_ = response.Body.Close()
	if candidate.Cookies == original.Cookies {
		return nil
	}
	newToken, err := verify(candidate)
	if code := failureStatus(err); code == http.StatusUnauthorized || code == http.StatusForbidden {
		return verifySaved(cookieCandidateRejected)
	}
	if err != nil {
		return err
	}
	if err = authCtx.Err(); err != nil {
		return err
	}
	if !r.cookieCCCurrent(id, configEpoch, epoch) {
		return staleCookieCC()
	}
	if catalog := original.Catalog(); len(catalog) != 0 {
		candidate.SetCatalog(catalog, original.CatalogUpdatedAt)
	}
	candidate.LastRenewedAt = r.now().UTC()
	candidate.LastCCRenewedAt = candidate.LastRenewedAt
	commitCtx := context.WithValue(context.WithoutCancel(ctx), cookieCCCommitKey{}, configEpoch)
	committed, err := r.commitCookieRotation(commitCtx, id, epoch, original, candidate)
	if err != nil {
		return err
	}
	*credential = committed
	epoch++
	status, verifiedToken = "renewed", newToken
	return nil
}

func (r *Runtime) recordCookieCCOutcome(id string, c Credential, route string, configEpoch, epoch uint64, started time.Time, status string, reason cookieCheckReason, verifiedToken string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cookieCCActiveLocked() || r.ccEpoch != configEpoch || r.credentialEpochs[id] != epoch {
		return
	}
	key := cookieCheckKey(id, c)
	check := r.cookieChecks[key]
	now := r.now()
	next := started.Add(cookieCCInterval)
	var loginErr *loginRequiredError
	switch {
	case errors.As(err, &loginErr):
		check.at = now
		r.markCookieReloginLocked(id, &check)
		status, reason, next = "paused", cookieSessionRejected, time.Time{}
	case err != nil || status == "retrying":
		status = "retrying"
		if err != nil {
			reason = cookieTemporaryFailure
			switch failureStatus(err) {
			case http.StatusTooManyRequests:
				reason = cookieRateLimited
			case http.StatusForbidden:
				reason = cookieAccessDenied
			}
		}
		backoff := cookieCheckBackoff
		var upstream *upstreamCookieFailure
		if errors.As(err, &upstream) {
			backoff = max(backoff, upstream.retry)
		}
		next = now.Add(backoff)
	case status == cookieCCUnsupported:
		next = time.Time{}
	}
	if verifiedToken != "" && check.status != "relogin_required" {
		check.verified = now
		r.tokens[cookieTokenKey(id, c, route)] = tokenState{verifiedToken, now.Add(240 * time.Second), now}
	}
	check.cc = cookieCCCheckState{epoch: configEpoch, status: status, at: now, next: next, reason: reason}
	r.cookieChecks[key] = check
}

func (r *Runtime) cookieCCStatusLocked(id string, c Credential, check cookieCheckState) map[string]any {
	status, reason := check.cc.status, check.cc.reason
	if status == "" || check.cc.epoch != r.ccEpoch {
		status, reason = "pending", ""
	}
	if !r.cookieCCActiveLocked() {
		status, reason = "paused", "polling_disabled"
	} else if check.status == "relogin_required" {
		status, reason = "paused", cookieSessionRejected
	}
	summary := map[string]any{"status": status}
	if reason != "" {
		summary["reason"] = reason
	}
	if !check.cc.at.IsZero() {
		summary["last_checked_at"] = check.cc.at
	}
	if next := r.cookieCCNextLocked(id, check); !next.IsZero() {
		summary["next_check_at"] = next
	}
	if !c.LastCCRenewedAt.IsZero() {
		summary["last_renewed_at"] = c.LastCCRenewedAt
	}
	return summary
}
