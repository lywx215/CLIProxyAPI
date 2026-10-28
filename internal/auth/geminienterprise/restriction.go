package geminienterprise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Restriction is a sanitized generation rejection, never a quota observation.
// No upstream messages, subjects, tokens or arbitrary metadata survive parsing.
type Restriction struct {
	Kind       string    `json:"kind"`
	Scope      string    `json:"scope"`
	Feature    string    `json:"feature"`
	Edition    string    `json:"edition,omitempty"`
	RetryAt    time.Time `json:"retry_at"`
	ResetAt    time.Time `json:"reset_at,omitempty"`
	TimeSource string    `json:"time_source"`
}

type GenerationError struct {
	HTTPStatus int
	// UpstreamHTTPStatus is the transport status, before decoding a stream error.
	UpstreamHTTPStatus int
	RPCStatus          string
	UpstreamCode       int
	Started            bool
	// ModelNotEnabled is set only from a validated Google ErrorInfo detail.
	ModelNotEnabled bool
	Model           string
	Restriction     *Restriction
	RetryAt         time.Time
	// Diagnostic contains only locally constructed public error details.
	Diagnostic error
	now        time.Time
}

// TransportError preserves network classification without exposing request details.
type TransportError struct {
	Cause   error
	Started bool
}

func (*TransportError) Error() string             { return "gemini-enterprise: upstream transport failed" }
func (e *TransportError) Unwrap() error           { return e.Cause }
func (e *TransportError) IsRequestScoped() bool   { return e.Started }
func (e *TransportError) GenerationStarted() bool { return e.Started }
func (e *TransportError) StatusCode() int {
	if errors.Is(e.Cause, context.Canceled) {
		return 499
	}
	if errors.Is(e.Cause, context.DeadlineExceeded) {
		return 504
	}
	return http.StatusBadGateway
}

func (e *GenerationError) Error() string {
	if detail, ok := e.Diagnostic.(interface{ PublicDiagnostic(int) string }); ok {
		if message := detail.PublicDiagnostic(e.HTTPStatus); message != "" {
			return message
		}
	}
	return fmt.Sprintf("gemini-enterprise: Google generation returned HTTP %d", e.HTTPStatus)
}
func (e *GenerationError) StatusCode() int         { return e.HTTPStatus }
func (e *GenerationError) GenerationStarted() bool { return e.Started }
func (e *GenerationError) IsRequestScoped() bool   { return e.Started || e.IsRequestStop() }
func (e *GenerationError) IsRequestStop() bool {
	if e.Model != "" && IsAutoModel(e.Model) {
		return true
	}
	if e.UpstreamHTTPStatus >= 200 && e.UpstreamHTTPStatus < 300 && e.RPCStatus == "" && e.UpstreamCode == 0 {
		return true
	}
	if !e.ModelNotEnabled && (e.HTTPStatus == 400 || e.HTTPStatus == 404 || e.HTTPStatus == 409 || e.HTTPStatus == 422 || e.HTTPStatus == 499 || e.HTTPStatus == 501) {
		return true
	}
	var stop interface{ IsRequestStop() bool }
	return errors.As(e.Diagnostic, &stop) && stop.IsRequestStop()
}
func (e *GenerationError) Unwrap() error { return e.Diagnostic }
func (e *GenerationError) RetryAfter() *time.Duration {
	if !e.RetryAt.After(e.now) {
		return nil
	}
	d := e.RetryAt.Sub(e.now)
	return &d
}

var textQuotaMetric = regexp.MustCompile(`^discoveryengine\.googleapis\.com/(?:quota/)?text_answer_gen_tier_([a-z_]+)_regional$`)
var technicalQuotaMetric = regexp.MustCompile(`^discoveryengine\.googleapis\.com/(?:quota/)?(?:requests|complete_query_requests|search_requests)(?:_regional)?$`)
var dailyQuotaLimit = regexp.MustCompile(`(?i)Per_?Day(?:_?Per_?(?:Project|User|Edition|Location|Region)|_?Regional)*$`)
var minuteQuotaLimit = regexp.MustCompile(`(?i)Per_?Minute(?:_?Per_?(?:Project|User|Edition|Location|Region)|_?Regional)*$`)
var userQuotaLimit = regexp.MustCompile(`(?i)Per_?User(?:_?Per|_?Regional|$)`)
var quotaRetryDelay = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,9})?s$`)
var numberProject = regexp.MustCompile(`^[1-9][0-9]*$`)

// NextPacificMidnight uses calendar arithmetic, including 23/25-hour DST days.
func NextPacificMidnight(now time.Time) time.Time {
	zone, _ := time.LoadLocation("America/Los_Angeles")
	local := now.In(zone)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, zone).UTC()
}

// ParseGenerationError recognizes only structured, resource-matching quota details.
// Unknown 429s retain the ordinary short backoff. Human descriptions are ignored.
func ParseGenerationError(raw []byte, status int, headers http.Header, binding config.GeminiEnterpriseOAuth, identity *QuotaIdentity, now time.Time, started bool) *GenerationError {
	e := &GenerationError{HTTPStatus: status, UpstreamHTTPStatus: status, Started: started, now: now}
	streamError := status == 0 || status >= 200 && status < 300
	if streamError {
		e.HTTPStatus = 502
	}
	retryAt := quotaHeaderRetryAt(headers, now)
	if status == 429 {
		e.RetryAt = retryAt
	}
	if IsModelNotEnabledResponse(raw, status) {
		e.HTTPStatus = http.StatusBadRequest
		e.ModelNotEnabled = true
	}
	type rpcStatus struct {
		Code    int               `json:"code"`
		Status  string            `json:"status"`
		Details []json.RawMessage `json:"details"`
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
		rpcStatus
	}
	if len(raw) > 1024*1024 || json.Unmarshal(raw, &envelope) != nil {
		return e
	}
	body := envelope.rpcStatus
	if len(envelope.Error) > 0 {
		body = rpcStatus{}
		if json.Unmarshal(envelope.Error, &body) != nil {
			return e
		}
	}
	if mapped, name := generationStatus(body.Code, body.Status); mapped != 0 {
		e.RPCStatus, e.UpstreamCode = name, body.Code
		if streamError {
			e.HTTPStatus = mapped
		}
	}
	if e.HTTPStatus != 429 {
		if e.HTTPStatus == 500 || e.HTTPStatus == 502 || e.HTTPStatus == 503 || e.HTTPStatus == 504 {
			e.RetryAt = retryAt
		}
		return e
	}
	e.RetryAt = retryAt
	if (body.Code != 0 && body.Code != 8 && body.Code != 429) || (body.Status != "" && body.Status != "RESOURCE_EXHAUSTED") {
		return e
	}
	binding = binding.WithDefaults()
	if !identity.ValidFor(binding) {
		identity = nil
	}
	type violation struct {
		Metric     string            `json:"quotaMetric"`
		ID         string            `json:"quotaId"`
		Dimensions map[string]string `json:"quotaDimensions"`
	}
	metadata := map[string]string{}
	var violations []violation
	resetAt := time.Time{}
	userDimension := false
	mergeTime := func(t time.Time) {
		if t.After(retryAt) {
			retryAt = t
		}
	}
	projectMatches := func(value string) bool {
		project := strings.TrimPrefix(value, "projects/")
		return project != "" && (project == binding.ProjectID || (identity != nil && project == identity.ProjectNumber))
	}
	// Only these structured fields can affect a restriction. Validate every alias
	// and detail before combining them; a later detail cannot hide a conflict.
	mergeMetadata := func(target, source map[string]string, dimensions bool) bool {
		for key, value := range source {
			if value == "" {
				if dimensions {
					return false
				}
				continue
			}
			switch key {
			case "consumer", "project", "project_number":
				if !projectMatches(value) || (key == "project_number" && !numberProject.MatchString(value)) {
					return false
				}
				target["project"] = binding.ProjectID
				continue
			case "service":
				if value != "discoveryengine.googleapis.com" {
					return false
				}
				continue
			case "location", "region", "regional_location":
				if value != binding.Location {
					return false
				}
				continue
			case "edition", "subscription_tier":
				value = restrictionEdition(value)
				if value == "" || (identity != nil && value != identity.Edition) {
					return false
				}
				key = "edition"
			case "feature", "quota_feature":
				if value != "assistant_search" && value != "technical_requests" {
					return false
				}
				key = "feature"
			case "quota_scope":
				if value == "shared_pool" {
					value = "edition"
				}
				if value != "user" && value != "project" && value != "edition" {
					return false
				}
			case "quota_metric":
				value = canonicalQuotaMetric(value)
			case "quota_limit":
			case "quota_unit":
				value = strings.ToLower(value)
			case "reset_time":
				if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil && stamp.After(now) && stamp.After(resetAt) {
					resetAt = stamp
				}
				continue
			case "user":
				// A dimension's arbitrary principal is not proof of the OAuth user.
				userDimension = true
				continue
			default:
				if dimensions {
					return false
				}
				continue
			}
			if previous := target[key]; previous != "" && previous != value {
				return false
			}
			target[key] = value
		}
		return true
	}
	validDetails := true
	for _, detail := range body.Details {
		var d struct {
			Type       string            `json:"@type"`
			Domain     string            `json:"domain"`
			Metadata   map[string]string `json:"metadata"`
			RetryDelay string            `json:"retryDelay"`
			Violations json.RawMessage   `json:"violations"`
		}
		if json.Unmarshal(detail, &d) != nil {
			validDetails = false
			continue
		}
		switch d.Type {
		case "type.googleapis.com/google.rpc.ErrorInfo":
			if (d.Domain != "googleapis.com" && d.Domain != "discoveryengine.googleapis.com") || !mergeMetadata(metadata, d.Metadata, false) {
				validDetails = false
			}
		case "type.googleapis.com/google.rpc.QuotaFailure":
			var items []violation
			if json.Unmarshal(d.Violations, &items) != nil {
				validDetails = false
			} else {
				violations = append(violations, items...)
			}
		case "type.googleapis.com/google.rpc.RetryInfo":
			if delay, err := time.ParseDuration(d.RetryDelay); quotaRetryDelay.MatchString(d.RetryDelay) && err == nil && delay > 0 {
				mergeTime(now.Add(delay))
			}
		}
	}
	e.RetryAt = retryAt
	if !validDetails {
		return e
	}
	if len(violations) == 0 {
		violations = append(violations, violation{Metric: metadata["quota_metric"], ID: metadata["quota_limit"]})
	}
	fieldsByViolation := make([]map[string]string, 0, len(violations))
	for _, v := range violations {
		fields := make(map[string]string, len(metadata)+2)
		for key, value := range metadata {
			fields[key] = value
		}
		if !mergeMetadata(fields, v.Dimensions, true) || !mergeMetadata(fields, map[string]string{"quota_metric": v.Metric, "quota_limit": v.ID}, false) {
			return e
		}
		fieldsByViolation = append(fieldsByViolation, fields)
	}
	// Multiple different violations cannot safely establish one restriction scope.
	var restriction *Restriction
	for _, fields := range fieldsByViolation {
		metric, limit := fields["quota_metric"], fields["quota_limit"]
		kind, userScope, unitValid := restrictionUnit(fields["quota_unit"])
		limitKind := ""
		switch {
		case minuteQuotaLimit.MatchString(limit):
			limitKind = "minute"
		case dailyQuotaLimit.MatchString(limit):
			limitKind = "daily"
		}
		if !unitValid || (kind != "" && limitKind != "" && kind != limitKind) {
			return e
		}
		if kind == "" {
			kind = limitKind
		}
		userScope = userScope || fields["quota_scope"] == "user" || (limitKind != "" && userQuotaLimit.MatchString(limit))
		matched := textQuotaMetric.FindStringSubmatch(metric)
		r := &Restriction{Kind: kind, Feature: "assistant_search", RetryAt: retryAt, TimeSource: "google"}
		switch {
		case kind == "minute":
			r.Scope = "project"
			r.Feature = "technical_requests"
			if len(matched) > 0 {
				r.Edition = matched[1]
				r.Scope = "edition"
				r.Feature = "assistant_search"
			}
			if len(matched) == 0 && !technicalQuotaMetric.MatchString(metric) {
				return e
			}
		case len(matched) > 0 && kind == "daily":
			r.Scope = "edition"
			r.Edition = matched[1]
		default:
			return e
		}
		if userScope {
			r.Scope = "user"
		}
		if fields["project"] == "" || (r.Edition != "" && (restrictionEdition(r.Edition) == "" || (identity != nil && r.Edition != identity.Edition))) {
			return e
		}
		if (fields["edition"] != "" && fields["edition"] != r.Edition) || (fields["feature"] != "" && fields["feature"] != r.Feature) || (fields["quota_scope"] != "" && fields["quota_scope"] != r.Scope) {
			return e
		}
		if userDimension {
			r.Scope = "credential"
		}
		r.ResetAt = resetAt
		if resetAt.After(r.RetryAt) {
			r.RetryAt = resetAt
		}
		if r.RetryAt.IsZero() {
			if r.Kind == "daily" {
				r.RetryAt = NextPacificMidnight(now)
				r.TimeSource = "estimated_pacific_midnight"
			} else {
				r.RetryAt = now.Add(time.Minute)
				r.TimeSource = "estimated_minute"
			}
		}
		if restriction != nil && (restriction.Kind != r.Kind || restriction.Scope != r.Scope || restriction.Edition != r.Edition || restriction.Feature != r.Feature) {
			return e
		}
		restriction = r
	}
	e.Restriction = restriction
	if restriction != nil {
		e.RetryAt = restriction.RetryAt
	}
	return e
}

func canonicalQuotaMetric(metric string) string {
	if strings.HasSuffix(metric, "/limit") {
		return strings.TrimSuffix(metric, "/limit")
	}
	return strings.TrimSuffix(metric, "/usage")
}

func restrictionEdition(value string) string {
	if edition := SubscriptionEdition(value); edition != "" {
		return edition
	}
	switch value {
	case "enterprise_plus", "enterprise_standard", "enterprise_emerging", "frontline_worker", "frontline_starter", "starter", "business_standard", "edu", "edu_pro", "edu_emerging", "edu_pro_emerging":
		return value
	}
	return ""
}

func restrictionUnit(unit string) (kind string, user bool, valid bool) {
	if unit == "" {
		return "", false, true
	}
	parts := strings.Split(unit, "/")
	if len(parts) < 2 {
		return "", false, false
	}
	if count, err := strconv.ParseUint(parts[0], 10, 64); err != nil || count == 0 {
		return "", false, false
	}
	switch parts[1] {
	case "min":
		kind = "minute"
	case "d":
		kind = "daily"
	default:
		return "", false, false
	}
	for _, dimension := range parts[2:] {
		switch dimension {
		case "{user}":
			user = true
		case "{project}", "{location}", "{region}", "{edition}", "{subscription_tier}":
		default:
			return "", false, false
		}
	}
	return kind, user, true
}

func quotaHeaderRetryAt(headers http.Header, now time.Time) time.Time {
	var retryAt time.Time
	for _, value := range headers.Values("Retry-After") {
		var stamp time.Time
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
			stamp = now.Add(time.Duration(seconds) * time.Second)
		} else if parsed, err := http.ParseTime(value); err == nil {
			stamp = parsed
		}
		if stamp.After(now) && stamp.After(retryAt) {
			retryAt = stamp
		}
	}
	return retryAt
}
