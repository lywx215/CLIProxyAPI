// Package diagnostics implements the frozen ai-proxy-diagnostics/1 HTTP profile.
// It observes local boundaries only; it does not infer retries or remote identity.
package diagnostics

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	customIDPattern = regexp.MustCompile(`\A[A-Za-z0-9._:/-]{1,128}\z`)
	tokenPattern    = regexp.MustCompile(`\A[A-Za-z0-9._-]+\z`)
	hexPattern      = regexp.MustCompile(`\A[0-9a-f]+\z`)
	stateKey        = regexp.MustCompile(`\A(?:[a-z][a-z0-9_*/-]{0,255}|[a-z0-9][a-z0-9_*/-]{0,240}@[a-z][a-z0-9_*/-]{0,13})\z`)
)

// HeaderValues preserves field multiplicity. net/http canonicalizes names on
// ingress; sorting also makes synthetic, mixed-case map inputs deterministic.
func HeaderValues(h http.Header, name string) []string {
	var keys []string
	for k := range h {
		if strings.EqualFold(k, name) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var values []string
	for _, k := range keys {
		values = append(values, h[k]...)
	}
	return values
}

func customID(values []string) (*string, string) {
	if len(values) == 0 {
		return nil, "missing"
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return nil, "duplicate"
	}
	if len(values[0]) > 128 {
		return nil, "too_long"
	}
	if !customIDPattern.MatchString(values[0]) {
		return nil, "invalid"
	}
	return &values[0], "none"
}

func nonzeroHex(s string, length int) bool {
	return len(s) == length && hexPattern.MatchString(s) && strings.Trim(s, "0") != ""
}

// Incoming is a parsed claim, not authenticated evidence of a remote call.
type Incoming struct {
	ContextSource   string  `json:"contextSource"`
	TraceID         string  `json:"traceId"`
	ParentSpanID    *string `json:"parentSpanId"`
	Flags           string  `json:"outputFlags"`
	Tracestate      *string `json:"tracestate"`
	CallerRequestID *string `json:"callerRequestId"`
	CallerHeader    *string `json:"callerHeader"`
	CallerTrust     string  `json:"callerTrust"`
	Rejected        string  `json:"rejected"`
}

// Extract applies W3C Level 1 plus the frozen local limits to framework fields.
// The caller must establish authentication locally before enabling the adapter.
func Extract(h http.Header, authenticated, configuredInboundAdapter bool) Incoming {
	x := Incoming{ContextSource: "generated", TraceID: randomHex(16), Flags: "00", CallerTrust: "none", Rejected: "none"}
	parents := HeaderValues(h, "traceparent")
	if len(parents) > 0 {
		x.ContextSource = "invalid_replaced"
	}
	if len(parents) == 1 {
		v := parents[0]
		if len(v) >= 55 && len(v) <= 512 && printable(v) && !strings.Contains(v, ",") &&
			v[2] == '-' && v[35] == '-' && v[52] == '-' && hexPattern.MatchString(v[:2]) && v[:2] != "ff" &&
			nonzeroHex(v[3:35], 32) && nonzeroHex(v[36:52], 16) && hexPattern.MatchString(v[53:55]) &&
			(len(v) == 55 || (v[:2] != "00" && v[55] == '-')) {
			x.ContextSource, x.TraceID = "accepted", v[3:35]
			parent := v[36:52]
			x.ParentSpanID = &parent
			flags, _ := strconv.ParseUint(v[53:55], 16, 8)
			if flags&1 != 0 {
				x.Flags = "01"
			}
			x.Tracestate = parseTracestate(HeaderValues(h, "tracestate"))
		}
	}
	names := []string{"x-request-id"}
	if authenticated && configuredInboundAdapter {
		names = []string{"x-diag-request-id", "x-request-id"}
	}
	for _, name := range names {
		id, reason := customID(HeaderValues(h, name))
		if reason != "none" && reason != "missing" && x.Rejected == "none" {
			x.Rejected = reason
		}
		if id != nil {
			x.CallerRequestID, x.CallerHeader = id, &name
			x.CallerTrust = "unverified"
			if authenticated {
				x.CallerTrust = "authenticated"
			}
			if name == "x-diag-request-id" {
				x.CallerTrust = "configured_peer"
			}
			break
		}
	}
	return x
}

func parseTracestate(values []string) *string {
	value := strings.Join(values, ",")
	if len(value) > 512 {
		return nil
	}
	var members []string
	seen := map[string]bool{}
	for _, member := range strings.Split(value, ",") {
		member = strings.Trim(member, " \t")
		if member == "" {
			continue
		}
		key, v, ok := strings.Cut(member, "=")
		if !ok || !stateKey.MatchString(key) || len(key) > 256 || len(v) == 0 || len(v) > 256 || !printable(v) || strings.Contains(v, "=") || seen[key] {
			return nil
		}
		seen[key] = true
		members = append(members, member)
	}
	if len(members) == 0 || len(members) > 32 {
		return nil
	}
	out := strings.Join(members, ",")
	return &out
}

func printable(v string) bool {
	for i := range len(v) {
		if v[i] < 32 || v[i] > 126 {
			return false
		}
	}
	return true
}

// IsPropagationHeader identifies fields excluded only at automatic copy sources.
func IsPropagationHeader(name string) bool {
	n := strings.ToLower(name)
	return n == "traceparent" || n == "tracestate" || strings.HasPrefix(n, "x-diag-")
}

// CopyInboundHeaders is only for established automatic inbound copy sites.
// Explicit provider header APIs must not call it indiscriminately.
func CopyInboundHeaders(source http.Header) http.Header {
	out := source.Clone()
	for name := range out {
		if IsPropagationHeader(name) {
			delete(out, name)
		}
	}
	return out
}

func cleanHeaders(h http.Header, owned bool) {
	for name := range h {
		n := strings.ToLower(name)
		if strings.HasPrefix(n, "x-diag-") || (owned && (n == "traceparent" || n == "tracestate")) {
			delete(h, name)
		}
	}
}

func inject(h http.Header, x Incoming, requestID, spanID string, peer, owned bool) bool {
	cleanHeaders(h, owned || peer)
	if peer {
		h.Set("Traceparent", "00-"+x.TraceID+"-"+spanID+"-"+x.Flags)
		if x.Tracestate != nil {
			h.Set("Tracestate", *x.Tracestate)
		}
		h.Set("X-Diag-Request-Id", requestID)
	}
	return peer
}

// ResponseHeaders replaces peer diagnostic fields without touching business IDs.
func ResponseHeaders(h http.Header, requestID, traceID string) {
	cleanHeaders(h, false)
	h.Set("X-Diag-Request-Id", requestID)
	h.Set("X-Diag-Trace-Id", traceID)
}

type PeerIDs struct {
	RequestID *string `json:"peerRequestId"`
	TraceID   *string `json:"peerTraceId"`
	Rejected  string  `json:"peerIdRejected"`
}

func ReadPeerIDs(h http.Header, configured bool) PeerIDs {
	x := PeerIDs{Rejected: "none"}
	if !configured {
		return x
	}
	var requestReason, traceReason string
	x.RequestID, requestReason = customID(HeaderValues(h, "x-diag-request-id"))
	x.TraceID, traceReason = customID(HeaderValues(h, "x-diag-trace-id"))
	if x.TraceID != nil && !nonzeroHex(*x.TraceID, 32) {
		x.TraceID, traceReason = nil, "invalid"
	}
	for _, reason := range []string{requestReason, traceReason} {
		if reason != "none" && reason != "missing" {
			x.Rejected = reason
			break
		}
	}
	return x
}
