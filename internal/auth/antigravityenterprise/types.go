// Package antigravityenterprise implements the Business AI Code subscription protocol.
package antigravityenterprise

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

const Provider = "antigravity-enterprise"
const AccountKey = "antigravity_enterprise_account"
const WeeklyBucket = "gemini-enterprise-weekly"
const CarbonModel = "gemini-4-carbon"
const QuotaTTL = 2 * time.Minute

var component = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type License struct {
	UserTier           string `json:"userTier"`
	TierDisplayName    string `json:"tierDisplayName"`
	ProjectID          string `json:"projectId"`
	Location           string `json:"location"`
	ProcessingLocation string `json:"processingLocation,omitempty"`
}

func (l License) Validate() error {
	if !component.MatchString(l.ProjectID) || !component.MatchString(l.UserTier) || Origin(l.Location) == "" {
		return fmt.Errorf("%s: invalid subscription binding", Provider)
	}
	if l.ProcessingLocation != "" && l.ProcessingLocation != l.Location {
		return fmt.Errorf("%s: unsupported processing location", Provider)
	}
	return nil
}
func (l License) Parent() string { return "projects/" + l.ProjectID + "/locations/" + l.Location }
func (l License) PoolKey() string {
	return strings.Join([]string{Provider, l.ProjectID, l.Location, l.UserTier, WeeklyBucket}, "|")
}
func Digest(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func AccountID(subject, client string) string { return Digest(Provider, "account", subject, client) }
func BindingID(account string, l License) string {
	return Digest(Provider, "binding", account, l.ProjectID, l.Location)
}

type Binding struct {
	ID       string         `json:"binding_id"`
	Revision uint64         `json:"revision"`
	License  License        `json:"license"`
	Disabled bool           `json:"disabled"`
	Missing  bool           `json:"missing,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}
type Discovery struct {
	Status      string    `json:"status"`
	CheckedAt   time.Time `json:"checked_at"`
	Unreachable []string  `json:"unreachable,omitempty"`
}
type Account struct {
	Version   int       `json:"version"`
	ID        string    `json:"account_id"`
	Revision  uint64    `json:"revision"`
	Bindings  []Binding `json:"bindings"`
	Discovery Discovery `json:"discovery"`
}

func ReadAccount(metadata map[string]any) (*Account, error) {
	raw, ok := metadata[AccountKey]
	if !ok {
		return nil, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid account", Provider)
	}
	var a Account
	if json.Unmarshal(b, &a) != nil || a.Version != 1 || a.ID == "" || a.Revision == 0 || len(a.Bindings) > 10000 {
		return nil, fmt.Errorf("%s: invalid account", Provider)
	}
	if String(metadata, "google_user_id") == "" || String(metadata, "client_id") == "" {
		return nil, fmt.Errorf("%s: account identity missing", Provider)
	}
	if a.ID != AccountID(String(metadata, "google_user_id"), String(metadata, "client_id")) {
		return nil, fmt.Errorf("%s: account identity mismatch", Provider)
	}
	seen := map[string]bool{}
	for _, binding := range a.Bindings {
		if binding.ID != BindingID(a.ID, binding.License) || seen[binding.ID] || binding.Revision == 0 || binding.License.Validate() != nil {
			return nil, fmt.Errorf("%s: invalid or duplicate binding", Provider)
		}
		seen[binding.ID] = true
	}
	return &a, nil
}
func String(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}
func PutAccount(m map[string]any, a Account) map[string]any {
	b, _ := json.Marshal(a)
	var graph map[string]any
	_ = json.Unmarshal(b, &graph)
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[AccountKey] = graph
	return out
}
func MergeLicenses(a Account, licenses []License, unreachable []string, now time.Time) (Account, error) {
	// Discovery ordering uses the request's start time, so a slow older request
	// cannot replace a later resource snapshot or mark its additions missing.
	if !a.Discovery.CheckedAt.IsZero() && now.Before(a.Discovery.CheckedAt) {
		return a, nil
	}
	old := map[string]Binding{}
	for _, v := range a.Bindings {
		old[v.ID] = v
	}
	seen := map[string]License{}
	for _, l := range licenses {
		if err := l.Validate(); err != nil {
			return a, err
		}
		id := BindingID(a.ID, l)
		if prior, ok := seen[id]; ok && prior != l {
			return a, fmt.Errorf("%s: conflicting subscription bindings", Provider)
		}
		seen[id] = l
	}
	for id, l := range seen {
		v, ok := old[id]
		if !ok {
			v = Binding{ID: id, Revision: 1, Settings: map[string]any{}}
		} else if v.License != l || v.Missing {
			v.Revision++
		}
		v.License, v.Missing = l, false
		old[id] = v
	}
	a.Bindings = nil
	for id, v := range old {
		if _, ok := seen[id]; !ok && len(unreachable) == 0 && !v.Missing {
			v.Missing = true
			v.Revision++
		}
		a.Bindings = append(a.Bindings, v)
	}
	sort.Slice(a.Bindings, func(i, j int) bool {
		x, y := a.Bindings[i].License, a.Bindings[j].License
		return x.ProjectID+"/"+x.Location < y.ProjectID+"/"+y.Location
	})
	a.Revision++
	status := "complete"
	if len(unreachable) > 0 {
		status = "partial"
	}
	a.Discovery = Discovery{Status: status, CheckedAt: now.UTC(), Unreachable: unreachable}
	return a, nil
}

type Config struct {
	AdminControls json.RawMessage `json:"adminControls,omitempty"`
	AllowedModels []string        `json:"allowedModels,omitempty"`
	Experiences   []Experience    `json:"availableExperiences,omitempty"`
}
type Experience struct {
	Name  string `json:"name"`
	Model struct {
		ID             string   `json:"modelId"`
		CanonicalID    string   `json:"canonicalModelId"`
		DisplayName    string   `json:"displayName"`
		ThinkingLevels []string `json:"supportedThinkingLevels"`
	} `json:"model"`
	Surface struct {
		Disabled bool `json:"disabled"`
	} `json:"surfaceConfig"`
}
type Model struct {
	ID         string `json:"id"`
	Upstream   string `json:"upstream_model"`
	Family     string `json:"model_family"`
	Canonical  string `json:"canonical_model"`
	Level      string `json:"thinking_level"`
	Experience string `json:"experience,omitempty"`
	Source     string `json:"source"`
}

func DefaultModels() []Model {
	out := []Model{}
	for _, family := range []string{"gemini-4-argon", "gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.1-pro"} {
		for _, level := range []string{"low", "medium", "high"} {
			if family == "gemini-3.1-pro" && level == "medium" {
				continue
			}
			canonical := family
			if family == "gemini-3.1-pro" {
				canonical += "-preview"
			}
			id := family + "-" + level
			// BAIC routes first-party requests by the complete native virtual ID.
			// The family and canonical IDs are metadata and policy identities only.
			out = append(out, Model{ID: id, Upstream: id, Family: family, Canonical: canonical, Level: level, Source: "official_default"})
		}
	}
	return out
}
func Models(c Config) []Model {
	defaults := DefaultModels()
	out := []Model{}
	for _, m := range defaults {
		allowed := len(c.AllowedModels) == 0
		for _, id := range c.AllowedModels {
			if modelMatchesID(m, id) {
				allowed = true
			}
		}
		if !allowed {
			continue
		}
		if len(c.Experiences) > 0 {
			matched := false
			for _, e := range c.Experiences {
				if !e.Surface.Disabled && (modelMatchesID(m, e.Model.ID) || modelMatchesID(m, e.Model.CanonicalID)) {
					if len(e.Model.ThinkingLevels) > 0 {
						supported := false
						for _, level := range e.Model.ThinkingLevels {
							if strings.TrimPrefix(strings.ToLower(level), "thinking_level_") == m.Level {
								supported = true
								break
							}
						}
						if !supported {
							continue
						}
					}
					m.Experience = e.Name
					m.Source = "fetch_config"
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

func modelMatchesID(model Model, id string) bool {
	return id != "" && (id == model.ID || id == model.Upstream || id == model.Family || id == model.Canonical)
}

type Bucket struct {
	ID        string       `json:"bucketId"`
	Window    string       `json:"window"`
	Fraction  *float64     `json:"remainingFraction,omitempty"`
	Amount    *json.Number `json:"remainingAmount,omitempty"`
	Disabled  bool         `json:"disabled,omitempty"`
	ResetTime *time.Time   `json:"resetTime,omitempty"`
}
type Quota struct {
	Groups []struct {
		DisplayName string   `json:"displayName"`
		Buckets     []Bucket `json:"buckets"`
	} `json:"groups"`
	Overage *struct {
		Enabled bool `json:"enabled"`
	} `json:"overageInfo"`
	ObservedAt time.Time `json:"observed_at"`
}

func (q Quota) Weekly() (*Bucket, error) {
	var found *Bucket
	for _, g := range q.Groups {
		for _, b := range g.Buckets {
			if b.ID != WeeklyBucket {
				continue
			}
			if found != nil {
				return nil, fmt.Errorf("conflicting weekly quota buckets")
			}
			copyBucket := b
			found = &copyBucket
		}
	}
	if found == nil || found.Window != "weekly" || found.Disabled || found.Fraction == nil && found.Amount == nil || found.Fraction != nil && found.Amount != nil {
		return nil, fmt.Errorf("weekly quota unavailable")
	}
	if found.Fraction != nil && (math.IsNaN(*found.Fraction) || math.IsInf(*found.Fraction, 0) || *found.Fraction < 0 || *found.Fraction > 1) {
		return nil, fmt.Errorf("invalid weekly quota fraction")
	}
	if found.Amount != nil {
		if n, err := found.Amount.Int64(); err != nil || n < 0 {
			return nil, fmt.Errorf("invalid weekly quota amount")
		}
	}
	return found, nil
}
func (b Bucket) Exhausted() bool {
	if b.Fraction != nil {
		return *b.Fraction == 0
	}
	if b.Amount != nil {
		v, _ := b.Amount.Int64()
		return v == 0
	}
	return false
}
func (q Quota) Admit(now time.Time) error {
	if q.ObservedAt.IsZero() || q.ObservedAt.After(now) || now.Sub(q.ObservedAt) >= QuotaTTL {
		return &Error{HTTP: 503, Code: "metadata_pending"}
	}
	if q.Overage == nil || q.Overage.Enabled {
		return &Error{HTTP: 409, Code: "subscription_only_required"}
	}
	b, err := q.Weekly()
	if err != nil {
		return &Error{HTTP: 503, Code: "quota_unknown"}
	}
	if b.ResetTime != nil && !b.ResetTime.After(now) {
		return &Error{HTTP: 503, Code: "metadata_pending"}
	}
	if b.Exhausted() {
		e := &Error{HTTP: 429, Code: "weekly_quota", Scope: "shared_pool"}
		if b.ResetTime != nil && b.ResetTime.After(now) {
			e.RetryAt = *b.ResetTime
		}
		return e
	}
	return nil
}

type Error struct {
	Cause     error  `json:"-"`
	Body      string `json:"-"`
	Attempted bool
	RetryHint bool
	HTTP      int
	Code      string
	Scope     string
	RetryAt   time.Time
	Started   bool
	RPCStatus string
}

func (e *Error) Unwrap() error        { return e.Cause }
func (e *Error) ResponseBody() []byte { return []byte(e.Body) }
func (e *Error) FailureScope() string { return e.Scope }
func (e *Error) FailureCode() string  { return e.Code }

func (e *Error) IsGenerationAttempted() bool { return e.Attempted }

func (e *Error) Error() string   { return fmt.Sprintf("%s: %s (HTTP %d)", Provider, e.Code, e.HTTP) }
func (e *Error) StatusCode() int { return e.HTTP }
func (e *Error) RetryAfter() *time.Duration {
	d := time.Until(e.RetryAt)
	if d <= 0 {
		return nil
	}
	return &d
}
func (e *Error) IsRequestScoped() bool {
	return e.Started || e.HTTP == 400 || e.HTTP == 409 || e.Code == "metadata_pending" || e.Code == "quota_unknown" || e.Code == "weekly_quota"
}
func (e *Error) GenerationStarted() bool { return e.Started }
func Origin(location string) string {
	switch location {
	case "global":
		return "https://businessaicode.googleapis.com"
	case "us", "eu":
		return "https://businessaicode." + location + ".rep.googleapis.com"
	}
	return ""
}
