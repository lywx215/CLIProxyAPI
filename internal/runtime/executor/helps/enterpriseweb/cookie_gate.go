package enterpriseweb

import (
	"math"

	"golang.org/x/sync/semaphore"
)

const allCookiePreparations = math.MaxInt64

// CookieSelection fences a queued authentication against changes to the selected auth.
type CookieSelection struct {
	ID                                   string
	RegistrationEpoch, CredentialVersion uint64
	Disabled                             bool
}

type cookieVersionRange struct{ first, latest uint64 }

// CanRebaseVerifiedCookies only admits a consecutive chain of committed rotations.
func (r *Runtime) CanRebaseVerifiedCookies(id string, selected, current uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.cookieVersions[id]
	return versions.first != 0 && selected >= versions.first && selected < current && current == versions.latest
}

// Authentication is coalesced per Cookie identity. Preparations share the Cookie
// snapshot through submission, and a writer waits for those readers to leave.
type cookieGate struct {
	auth, preparations *semaphore.Weighted
	refs               int
	attempts           map[string]*cookieAttempt // guarded by Runtime.mu
}

type cookieAttempt struct {
	waiters   int
	failedKey string
	err       error
}

// Join only overlaps verification already in progress. A later request can
// retry after a failure, while existing waiters share that failure.
func (r *Runtime) joinCookieAttempt(gate *cookieGate, key string) (*cookieAttempt, func()) {
	r.mu.Lock()
	if gate.attempts == nil {
		gate.attempts = make(map[string]*cookieAttempt)
	}
	attempt := gate.attempts[key]
	if attempt == nil || attempt.err != nil {
		attempt = &cookieAttempt{}
		gate.attempts[key] = attempt
	}
	attempt.waiters++
	r.mu.Unlock()
	return attempt, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		attempt.waiters--
		if attempt.waiters == 0 && gate.attempts[key] == attempt {
			delete(gate.attempts, key)
		}
	}
}

func (r *Runtime) retainCookieGate(key string) (*cookieGate, func()) {
	r.mu.Lock()
	gate := r.cookieGates[key]
	if gate == nil {
		gate = &cookieGate{auth: semaphore.NewWeighted(1), preparations: semaphore.NewWeighted(allCookiePreparations)}
		r.cookieGates[key] = gate
	}
	gate.refs++
	r.mu.Unlock()
	return gate, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		gate.refs--
		if gate.refs == 0 {
			delete(r.cookieGates, key)
		}
	}
}
