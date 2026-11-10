package enterpriseweb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

const ccFixtureID = "test-cc-credential"
const ccFixtureBootstrap = `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`

func cookieCCFixture(t *testing.T) (*Runtime, *Credential, *time.Time, *int) {
	t.Helper()
	r := NewRuntime()
	now := time.Date(2026, 11, 10, 4, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, CookieCCPollingEnabled: true})
	c := credentialFixture(t)
	if err := c.SetCookies(c.Cookies + "; __Secure-1PSIDTS=synthetic-ts"); err != nil {
		t.Fatal(err)
	}
	c.SetCatalog(c.AvailableModels, now)
	writes := 0
	r.SetCookieRotationCommitter(func(ctx context.Context, id string, epoch uint64, before, after Credential) (Credential, error) {
		release, err := r.LockCookieCCCommit(ctx)
		if err != nil {
			return Credential{}, err
		}
		if release != nil {
			defer release()
		}
		if r.CredentialEpoch(id) != epoch {
			return Credential{}, staleCookieCC()
		}
		writes++
		after.Revision = before.Revision + 1
		r.ReplaceVerifiedCookies(id, before, after, before.Revision, after.Revision, release != nil)
		return after, nil
	})
	return r, &c, &now, &writes
}

func ccFixtureResponse(status int, body string, cookies ...string) *http.Response {
	r := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	for _, cookie := range cookies {
		r.Header.Add("Set-Cookie", cookie)
	}
	return r
}

func ccSummary(r *Runtime, c Credential) map[string]any {
	return r.CredentialStatus(ccFixtureID, c)["cookie_cc_check"].(map[string]any)
}

func TestCookieCCPollKeepsIndependentDeadlines(t *testing.T) {
	r, c, now, writes := cookieCCFixture(t)
	var calls []string
	var ccValue = "first"
	var tsValue = "synthetic-ts"
	client := &http.Client{Transport: syntheticTransport(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.String() == googleCookieRotationURL:
			calls = append(calls, "ts")
			return ccFixtureResponse(200, `{}`, "__Secure-1PSIDTS="+tsValue+"; Domain=.google.com; Path=/; Secure"), nil
		case req.Method == http.MethodGet && req.URL.String() == c.PageURL:
			calls = append(calls, "home")
			return ccFixtureResponse(200, ccFixtureBootstrap), nil
		case req.URL.String() == c.APIBase()+"lookupWidgetConfig":
			calls = append(calls, "lookup")
			return ccFixtureResponse(200, syntheticPlusConfig), nil
		case req.URL.String() == c.APIBase()+widgetCCAction:
			calls = append(calls, "cc")
			raw, err := io.ReadAll(req.Body)
			query := gjson.GetBytes(raw, "advancedCompleteQueryRequest.query")
			if err != nil || !query.Exists() || query.String() != "" || gjson.GetBytes(raw, "configId").String() != c.ConfigID || req.Method != http.MethodPost || req.Header.Get("X-Goog-Api-Key") != "synthetic-api-key" || req.Header.Get("X-Server-Token") != "" {
				t.Error("poll did not use the scoped empty query and existing authentication")
			}
			return ccFixtureResponse(200, `{}`,
				"SIDCC="+ccValue+"; Domain=.google.com; Path=/; Secure; Max-Age=3600",
				"__Secure-1PSIDCC=synthetic-first; Domain=.google.com; Path=/; Secure",
				"__Secure-3PSIDCC=synthetic-third; Domain=.google.com; Path=/; Secure",
				"__Secure-1PSIDTS=forbidden; Domain=.google.com; Path=/; Secure",
				"__Secure-1PSIDRTS=forbidden; Domain=.google.com; Path=/; Secure",
				"SID=forbidden; Domain=.google.com; Path=/; Secure"), nil
		default:
			t.Errorf("unexpected operation %s", req.URL.Path)
			return ccFixtureResponse(500, `{}`), nil
		}
	})}
	selection := CookieSelection{ID: ccFixtureID}
	run := func(want ...string) {
		t.Helper()
		calls = nil
		if err := r.CheckCookie(t.Context(), c, client, selection, ""); err != nil || !slices.Equal(calls, want) {
			t.Fatalf("check operations = %v, want %v; error %v", calls, want, err)
		}
	}
	run("ts", "home", "lookup")
	tsDue := now.Add(cookieCheckInterval + cookieCheckOffset(ccFixtureID))
	ccDue := now.Add(cookieCCOffset(ccFixtureID))
	if next, ok := r.NextCookieCheck(ccFixtureID, *c, *now); !ok || next != ccDue {
		t.Fatalf("startup CC check = %v, want %v", next, ccDue)
	}
	oldModelEpoch := r.EvidenceEpoch(ccFixtureID)
	r.RecordCredentialModel(ccFixtureID, *c, "observed-web-id", oldModelEpoch)
	*now = ccDue
	run("cc", "home", "lookup")
	if *writes != 1 || c.Revision != 2 || c.LastCCRenewedAt != *now || strings.Contains(c.Cookies, "forbidden") || !strings.Contains(c.Cookies, "__Secure-1PSIDTS=synthetic-ts") || len(c.Catalog()) != 1 {
		t.Fatal("CC did not commit only the validated whitelist and preserve catalog")
	}
	r.RecordCredentialModel(ccFixtureID, *c, "late-result", oldModelEpoch)
	state := r.CredentialStatus(ccFixtureID, *c)
	if !slices.Equal(state["tested_model_ids"].([]string), []string{"observed-web-id"}) || state["cookie_next_check_at"] != tsDue || ccSummary(r, *c)["status"] != "renewed" {
		t.Fatal("renewal altered PSIDTS deadline, discarded evidence, or accepted a late result", state)
	}
	*now = now.Add(time.Minute)
	run("cc")
	if *writes != 1 || ccSummary(r, *c)["status"] != "no_change" {
		t.Fatal("unchanged Cookie wrote a revision or reported renewal")
	}
	*now = now.Add(time.Minute)
	ccValue = "second"
	run("cc", "home", "lookup")
	if *writes != 2 || c.Revision != 3 {
		t.Fatal("second CC rotation did not persist independently")
	}
	ccDue = now.Add(time.Minute)
	*now = tsDue
	tsValue = "new-ts"
	run("ts", "home", "lookup")
	if *writes != 3 || ccSummary(r, *c)["next_check_at"] != ccDue {
		t.Fatal("PSIDTS did not win the due tie and preserve the CC deadline")
	}
}

func TestCookieCCPollBackoffAndSessionConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, status           string
		poll, saved, candidate int
		retry                  string
		reason                 cookieCheckReason
		next                   time.Duration
		confirmCalls           int
	}{
		{"widget 401, saved valid", "retrying", 401, 200, 200, "", cookieRotationUnauthorized, 5 * time.Minute, 1},
		{"widget 403, saved valid", "retrying", 403, 200, 200, "", cookieAccessDenied, 5 * time.Minute, 1},
		{"candidate rejected", "retrying", 200, 200, 401, "", cookieCandidateRejected, 5 * time.Minute, 2},
		{"saved rejected", "paused", 401, 401, 200, "", cookieSessionRejected, 0, 1},
		{"rate limited", "retrying", 429, 200, 200, "720", cookieRateLimited, 12 * time.Minute, 0},
		{"upstream unavailable", "retrying", 503, 200, 200, "600", cookieTemporaryFailure, 10 * time.Minute, 0},
		{"unsupported", "unsupported", 404, 200, 200, "", "endpoint_unsupported", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, now, writes := cookieCCFixture(t)
			r.recordCookieOutcome(ccFixtureID, 0, *c, nil, true, "")
			r.tokens[cookieTokenKey(ccFixtureID, *c, "")] = tokenState{`{"api_key":"synthetic-api-key"}`, now.Add(4 * time.Minute), *now}
			*now = now.Add(cookieCCOffset(ccFixtureID))
			verifyCount := 0
			client := &http.Client{Transport: syntheticTransport(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.URL.String() == c.PageURL:
					verifyCount++
					status := tc.saved
					if strings.Contains(req.Header.Get("Cookie"), "SIDCC=synthetic-new") {
						status = tc.candidate
					}
					return ccFixtureResponse(status, ccFixtureBootstrap), nil
				case strings.HasSuffix(req.URL.Path, "/lookupWidgetConfig"):
					return ccFixtureResponse(200, syntheticPlusConfig), nil
				case strings.HasSuffix(req.URL.Path, "/"+widgetCCAction):
					res := ccFixtureResponse(tc.poll, `{}`, "SIDCC=synthetic-new; Domain=.google.com; Path=/; Secure")
					res.Header.Set("Retry-After", tc.retry)
					return res, nil
				default:
					t.Errorf("unexpected operation %s", req.URL.Path)
					return ccFixtureResponse(500, `{}`), nil
				}
			})}
			_ = r.CheckCookie(t.Context(), c, client, CookieSelection{ID: ccFixtureID}, "")
			s := ccSummary(r, *c)
			if *writes != 0 || verifyCount != tc.confirmCalls || s["status"] != tc.status || s["reason"] != tc.reason {
				t.Fatal("unexpected rejected-candidate outcome", s, verifyCount, *writes)
			}
			if tc.next > 0 && s["next_check_at"] != now.Add(tc.next) {
				t.Fatal("wrong retry deadline", s)
			}
			if _, ok := r.NextCookieCheck(ccFixtureID, *c, *now); ok == (tc.status == "paused") {
				t.Fatal("wrong suspension of complete credential checks")
			}
			if tc.status == "unsupported" || tc.status == "paused" {
				r.recordCookieOutcome(ccFixtureID, 0, *c, nil, false, "")
				if s := ccSummary(r, *c); s["status"] != "pending" || s["next_check_at"] == nil {
					t.Fatal("successful manual verification did not rearm CC", s)
				}
			}
		})
	}
}

func TestCookieCCPollDefersBusyAndDiscardsStaleResults(t *testing.T) {
	for _, change := range []string{"busy", "switch off and on", "replace or remove"} {
		t.Run(change, func(t *testing.T) {
			r, c, now, writes := cookieCCFixture(t)
			r.recordCookieOutcome(ccFixtureID, 0, *c, nil, true, "")
			r.tokens[cookieTokenKey(ccFixtureID, *c, "")] = tokenState{`{"api_key":"synthetic-api-key"}`, now.Add(4 * time.Minute), *now}
			*now = now.Add(cookieCCOffset(ccFixtureID))
			if change == "busy" {
				gate, release := r.retainCookieGate(c.CookieKey())
				defer release()
				if !gate.preparations.TryAcquire(1) {
					t.Fatal("could not establish in-flight preparation")
				}
				defer gate.preparations.Release(1)
			}
			calls := 0
			client := &http.Client{Transport: syntheticTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if change == "switch off and on" {
					r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true})
					r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, CookieCCPollingEnabled: true})
				} else {
					r.ForgetCredential(ccFixtureID)
				}
				return ccFixtureResponse(200, `{}`, "SIDCC=synthetic-late; Domain=.google.com; Path=/; Secure"), nil
			})}
			err := r.CheckCookie(t.Context(), c, client, CookieSelection{ID: ccFixtureID}, "")
			wantStatus, wantCalls := 409, 1
			if change == "busy" {
				wantStatus, wantCalls = 0, 0
				next, _ := r.NextCookieCheck(ccFixtureID, *c, *now)
				if !next.After(*now) || next.After(now.Add(4*time.Second)) || r.Status()["cookie_cc_polling_deferred"] != uint64(1) {
					t.Fatal("busy CC did not shift out of the collision")
				}
			}
			if failureStatus(err) != wantStatus || *writes != 0 || calls != wantCalls {
				t.Fatal("check used stale or busy credential", err, calls, *writes)
			}
			if ccSummary(r, *c)["last_checked_at"] != nil {
				t.Fatal("late result contaminated CC status")
			}
		})
	}
}

func TestCookieCCPollStaggeredCapacity(t *testing.T) {
	for _, count := range []int{1, 16} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r, base, now, writes := cookieCCFixture(t)
			start := *now
			type account struct {
				id         string
				credential Credential
				last, wait time.Time
				polls      int
			}
			accounts := make([]account, count)
			for i := range accounts {
				a := &accounts[i]
				a.id, a.credential = fmt.Sprintf("synthetic-capacity-%d", i), *base
				r.recordCookieOutcome(a.id, 0, a.credential, nil, true, "")
				r.tokens[cookieTokenKey(a.id, a.credential, "")] = tokenState{`{"api_key":"synthetic-api-key"}`, now.Add(4 * time.Minute), *now}
			}
			var intervals []time.Duration
			var slotUntil time.Time
			var release func()
			var selected *account
			calls, requests := 0, 0
			client := &http.Client{Transport: syntheticTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				switch {
				case req.URL.String() == googleCookieRotationURL:
					return ccFixtureResponse(200, `{}`), nil
				case req.URL.String() == base.PageURL:
					return ccFixtureResponse(200, ccFixtureBootstrap), nil
				case strings.HasSuffix(req.URL.Path, "/lookupWidgetConfig"):
					return ccFixtureResponse(200, syntheticPlusConfig), nil
				case strings.HasSuffix(req.URL.Path, "/"+widgetCCAction):
					requests++
					selected.polls++
					if !selected.last.IsZero() {
						intervals = append(intervals, now.Sub(selected.last))
					}
					selected.last = *now
					if selected.polls%2 == 0 {
						return ccFixtureResponse(200, `{}`, fmt.Sprintf("SIDCC=synthetic-%d; Domain=.google.com; Path=/; Secure", selected.polls)), nil
					}
					return ccFixtureResponse(200, `{}`), nil
				default:
					t.Fatalf("unexpected capacity operation %s", req.URL.Path)
					return nil, nil
				}
			})}
			for {
				var due time.Time
				for i := range accounts {
					a := &accounts[i]
					next, ok := r.NextCookieCheck(a.id, a.credential, *now)
					if !ok {
						t.Fatal("healthy fixture stopped scheduling")
					}
					if a.wait.After(next) {
						next = a.wait
					}
					if due.IsZero() || next.Before(due) {
						due, selected = next, a
					}
				}
				if due.Sub(start) >= 12*time.Minute {
					break
				}
				*now = due
				if release != nil && !now.Before(slotUntil) {
					release()
					release = nil
				}
				claimed := r.ClaimCookieCheck()
				if claimed == nil {
					if !r.DeferCookieCC(selected.id, selected.credential) {
						selected.wait = now.Add(time.Minute)
					}
					continue
				}
				release = claimed
				before := calls
				if err := r.CheckCookie(t.Context(), &selected.credential, client, CookieSelection{ID: selected.id}, ""); err != nil {
					t.Fatal(err)
				}
				slotUntil = now.Add(time.Duration(calls-before) * 150 * time.Millisecond)
			}
			if release != nil {
				release()
			}
			for _, a := range accounts {
				if a.polls < 10 {
					t.Fatalf("credential %s starved at %d polls", a.id, a.polls)
				}
			}
			slices.Sort(intervals)
			t.Logf("12 simulated minutes, 150 ms per upstream call: %d polls, %d saves, %d deferrals, interval p50 %s p95 %s max %s",
				requests, *writes, r.Status()["cookie_cc_polling_deferred"], intervals[len(intervals)/2], intervals[(len(intervals)-1)*95/100], intervals[len(intervals)-1])
		})
	}
}
