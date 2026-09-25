package diagnostics

import (
	"encoding/json"
	"io"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type Peer struct {
	Alias        string  `json:"alias"`
	Origin       string  `json:"origin"`
	PathPrefix   string  `json:"pathPrefix"`
	Service      string  `json:"service"`
	DeploymentID *string `json:"deploymentId"`
	origin       originKey
}

type originKey struct {
	scheme, host string
	port         int
}

// Peers is immutable after parsing and safe to publish as one atomic snapshot.
type Peers struct {
	entries []Peer
	Status  string
}

var (
	dnsLabel      = regexp.MustCompile(`\A[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\z`)
	numericLabel  = regexp.MustCompile(`\A(?:0x[0-9a-f]+|[0-9]+)\z`)
	prefixPattern = regexp.MustCompile(`\A/(?:[A-Za-z0-9._~-]+(?:/[A-Za-z0-9._~-]+)*)?\z`)
)

func validToken(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && tokenPattern.MatchString(s)
}

// ParsePeers disables the whole snapshot on any malformed entry, including
// duplicate JSON keys. Error text and configuration values never enter logs.
func ParsePeers(raw string) *Peers {
	bad := &Peers{Status: "config_invalid"}
	if raw == "" {
		raw = "[]"
	}
	// Bound work before decoding. A valid 64-entry configuration fits this limit.
	if len(raw) > 128*1024 {
		return bad
	}
	d := json.NewDecoder(strings.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('[') {
		return bad
	}
	out := &Peers{Status: "valid"}
	aliases := map[string]bool{}
	for d.More() {
		if len(out.entries) == 64 {
			return bad
		}
		tok, err = d.Token()
		if err != nil || tok != json.Delim('{') {
			return bad
		}
		fields := map[string]json.RawMessage{}
		for d.More() {
			t, e := d.Token()
			if e != nil {
				return bad
			}
			key, ok := t.(string)
			if !ok {
				return bad
			}
			if _, exists := fields[key]; exists {
				return bad
			}
			var v json.RawMessage
			if d.Decode(&v) != nil {
				return bad
			}
			fields[key] = v
		}
		if tok, err = d.Token(); err != nil || tok != json.Delim('}') || len(fields) != 5 {
			return bad
		}
		var p Peer
		for key, dest := range map[string]*string{"alias": &p.Alias, "origin": &p.Origin, "pathPrefix": &p.PathPrefix, "service": &p.Service} {
			v, ok := fields[key]
			if !ok || string(v) == "null" || json.Unmarshal(v, dest) != nil {
				return bad
			}
		}
		v, ok := fields["deploymentId"]
		if !ok || json.Unmarshal(v, &p.DeploymentID) != nil {
			return bad
		}
		if !validToken(p.Alias, 64) || aliases[p.Alias] || (p.DeploymentID != nil && !validToken(*p.DeploymentID, 64)) ||
			(p.Service != "cliproxyapi" && p.Service != "gcli2api" && p.Service != "aitoapi") ||
			len(p.Origin) < 8 || len(p.Origin) > 256 || len(p.PathPrefix) > 256 || !prefixPattern.MatchString(p.PathPrefix) || !safePath(p.PathPrefix) {
			return bad
		}
		if !strings.HasPrefix(p.Origin, "https://") && !strings.HasPrefix(p.Origin, "http://") {
			return bad
		}
		u, e := url.Parse(p.Origin)
		if e != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery {
			return bad
		}
		p.origin, ok = parseOrigin(u)
		if !ok {
			return bad
		}
		for _, existing := range out.entries {
			if p.origin == existing.origin && (prefixMatches(p.PathPrefix, existing.PathPrefix) || prefixMatches(existing.PathPrefix, p.PathPrefix)) {
				return bad
			}
		}
		aliases[p.Alias] = true
		out.entries = append(out.entries, p)
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim(']') {
		return bad
	}
	if _, err = d.Token(); err != io.EOF {
		return bad
	}
	return out
}

func parseOrigin(u *url.URL) (originKey, bool) {
	var zero originKey
	if u == nil || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" || !printable(u.String()) || strings.ContainsAny(u.String(), "\\#") {
		return zero, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return zero, false
	}
	if strings.ContainsAny(u.Host, "%@ ") || strings.HasSuffix(u.Host, ":") {
		return zero, false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasSuffix(host, ".") {
		return zero, false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" || (addr.Is6() && !strings.HasPrefix(u.Host, "[")) || (addr.Is4() && strings.HasPrefix(u.Host, "[")) {
			return zero, false
		}
		// netip compares address values, including expanded/mapped IPv6 spellings.
		host = addr.String()
	} else {
		if strings.Contains(host, ":") || len(host) > 253 {
			return zero, false
		}
		allNumeric := true
		for _, label := range strings.Split(host, ".") {
			if !dnsLabel.MatchString(label) {
				return zero, false
			}
			allNumeric = allNumeric && numericLabel.MatchString(label)
		}
		if allNumeric {
			return zero, false
		}
	}
	port := 80
	if scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		n, e := strconv.Atoi(u.Port())
		if e != nil || n < 1 || n > 65535 {
			return zero, false
		}
		port = n
	}
	return originKey{scheme, host, port}, true
}

func safePath(path string) bool {
	if !strings.HasPrefix(path, "/") || !printable(path) || strings.ContainsAny(path, "%\\ ") {
		return false
	}
	if path == "/" {
		return true
	}
	for _, segment := range strings.Split(strings.TrimSuffix(path[1:], "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func prefixMatches(prefix, path string) bool {
	return prefix == "/" || path == prefix || strings.HasPrefix(path, prefix+"/")
}

// Match evaluates the URL at the final RoundTrip boundary, never a base URL.
func (p *Peers) Match(u *url.URL) *Peer {
	if p == nil {
		return nil
	}
	origin, ok := parseOrigin(u)
	if !ok {
		return nil
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if !safePath(path) {
		return nil
	}
	for _, entry := range p.entries {
		if entry.origin == origin && prefixMatches(entry.PathPrefix, path) {
			return &entry
		}
	}
	return nil
}
