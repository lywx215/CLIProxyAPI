// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

type ErrorKind string
type ErrorStage string

const (
	ErrorLocalParameter   ErrorKind  = "local_parameter"
	ErrorLocalResource    ErrorKind  = "local_resource"
	ErrorClientCancelled  ErrorKind  = "client_cancelled"
	ErrorClientWrite      ErrorKind  = "client_write"
	ErrorAuthentication   ErrorKind  = "authentication"
	ErrorPermissionDenied ErrorKind  = "permission_denied"
	ErrorQuotaExhausted   ErrorKind  = "quota_exhausted"
	ErrorTransient        ErrorKind  = "transient"
	ErrorProtocol         ErrorKind  = "protocol"
	ErrorUpstreamRejected ErrorKind  = "upstream_rejected"
	StagePreflight        ErrorStage = "preflight"
	StageAuth             ErrorStage = "auth"
	StageSession          ErrorStage = "session"
	StageUpload           ErrorStage = "upload"
	StageGenerate         ErrorStage = "generate"
	StageStream           ErrorStage = "stream"
	maxErrorBody                     = 64 << 10
	maxLocalBackoff                  = 5 * time.Minute
	maxServerRetryAfter              = 24 * time.Hour
)

// OperationError exposes only safe classifications. Its unwrap remains a
// contract.Failure so existing native SDK error encoding continues to work.
// Submitted is conservative: a generation was handed to HTTP transport, and a
// network failure may leave its submission outcome unknown. No retry follows.
type OperationError struct {
	Kind              ErrorKind  `json:"kind"`
	Stage             ErrorStage `json:"stage"`
	Status            string     `json:"status,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	HTTPStatus        int        `json:"http_status,omitempty"`
	Submitted         bool       `json:"submitted"`
	OutputStarted     bool       `json:"output_started"`
	RetryAfterSeconds int64      `json:"retry_after_seconds,omitempty"`
	failure           *contract.Failure
	cause             error
}

func (e *OperationError) Error() string        { return e.failure.Message }
func (e *OperationError) Unwrap() error        { return e.failure }
func (e *OperationError) Is(target error) bool { return errors.Is(e.cause, target) }
func (e *OperationError) As(target any) bool   { return errors.As(e.cause, target) }

type responseError struct {
	status             int
	httpStatus         int
	statusEnum, reason string
	retryAfter         time.Duration
}

func (*responseError) Error() string { return "Business upstream rejected an operation" }

type protocolError struct{ cause error }

func (*protocolError) Error() string   { return "Business upstream response is malformed" }
func (e *protocolError) Unwrap() error { return e.cause }

type deliveryError struct{ cause error }

func (*deliveryError) Error() string   { return "Business response delivery failed" }
func (e *deliveryError) Unwrap() error { return e.cause }

type transportError struct{ cause error }

func (*transportError) Error() string   { return "Business upstream transport failed" }
func (e *transportError) Unwrap() error { return e.cause }

type rejectedStateError struct{ state string }

func (*rejectedStateError) Error() string { return "Business upstream did not complete the answer" }

type backoffPolicy struct {
	now    func() time.Time
	random func() float64
}

func defaultBackoffPolicy() backoffPolicy {
	return backoffPolicy{now: time.Now, random: mathrand.Float64}
}

func (p backoffPolicy) delay(kind ErrorKind, failures int, server time.Duration) time.Duration {
	base := 2 * time.Second
	if kind == ErrorQuotaExhausted {
		base = 15 * time.Second
	}
	for i := 1; i < failures && base < maxLocalBackoff; i++ {
		base = min(base*2, maxLocalBackoff)
	}
	value := p.random()
	if math.IsNaN(value) || value < 0 {
		value = 0
	}
	if value > 1 {
		value = 1
	}
	delay := base/2 + time.Duration(float64(base-base/2)*value)
	return max(delay, min(max(server, 0), maxServerRetryAfter))
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if len(value) > 128 {
		return 0
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	digits := true
	for _, c := range value {
		if c < '0' || c > '9' {
			digits = false
			break
		}
	}
	if digits {
		number, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return maxServerRetryAfter
		}
		if number > uint64(maxServerRetryAfter/time.Second) {
			return maxServerRetryAfter
		}
		return time.Duration(number) * time.Second
	}
	at, err := http.ParseTime(value)
	if err != nil || !at.After(now) {
		return 0
	}
	return min(at.Sub(now), maxServerRetryAfter)
}

var statuses = map[string]bool{
	"OK": true, "CANCELLED": true, "UNKNOWN": true, "INVALID_ARGUMENT": true, "DEADLINE_EXCEEDED": true,
	"NOT_FOUND": true, "ALREADY_EXISTS": true, "PERMISSION_DENIED": true, "RESOURCE_EXHAUSTED": true,
	"FAILED_PRECONDITION": true, "ABORTED": true, "OUT_OF_RANGE": true, "UNIMPLEMENTED": true,
	"INTERNAL": true, "UNAVAILABLE": true, "DATA_LOSS": true, "UNAUTHENTICATED": true,
}
var reasons = map[string]bool{
	"ACCESS_TOKEN_EXPIRED": true, "ACCESS_TOKEN_INVALID": true, "CREDENTIALS_MISSING": true, "INVALID_CREDENTIALS": true,
	"IAM_PERMISSION_DENIED": true, "ACCESS_DENIED": true, "SERVICE_DISABLED": true, "MODEL_ACCESS_DENIED": true,
	"MODEL_NOT_FOUND": true, "MODEL_NOT_SUPPORTED": true, "PERMISSION_DENIED": true, "BILLING_DISABLED": true,
	"RATE_LIMIT_EXCEEDED": true, "QUOTA_EXCEEDED": true, "RESOURCE_EXHAUSTED": true, "CONCURRENT_LIMIT_EXCEEDED": true,
	"INTERNAL_ERROR": true, "BACKEND_ERROR": true, "SERVICE_UNAVAILABLE": true,
	"FAILED": true, "SKIPPED": true, "CANCELLED": true,
}

func knownStatus(value string) string {
	if statuses[value] {
		return value
	}
	return ""
}
func knownReason(value string) string {
	if reasons[value] {
		return value
	}
	return ""
}

// Parse only the fixed error envelope, canonical status and ErrorInfo.reason.
// Message, metadata, cookies, URLs, tokens and unknown strings are never saved.
func parseRPCError(raw []byte) responseError {
	var result responseError
	if len(raw) > maxErrorBody {
		return result
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return result
	}
	var visit func(any, int)
	visit = func(item any, depth int) {
		if depth > 4 {
			return
		}
		switch v := item.(type) {
		case []any:
			for i, child := range v {
				if i >= 16 {
					break
				}
				visit(child, depth+1)
			}
		case map[string]any:
			if nested, ok := v["error"]; ok {
				visit(nested, depth+1)
				return
			}
			if text, ok := v["status"].(string); ok {
				if status := knownStatus(text); status != "" {
					result.statusEnum = status
				}
			}
			if number, ok := v["code"].(json.Number); ok {
				if code, err := strconv.Atoi(string(number)); err == nil {
					if code >= 400 && code <= 599 {
						result.status = code
					} else if status := grpcStatus(code); status != "" {
						result.status = httpForStatus(status)
						if result.statusEnum == "" {
							result.statusEnum = status
						}
					}
				}
			}
			if details, ok := v["details"].([]any); ok {
				for i, detail := range details {
					if i >= 16 {
						break
					}
					fields, ok := detail.(map[string]any)
					if !ok {
						continue
					}
					if fields["@type"] != "type.googleapis.com/google.rpc.ErrorInfo" {
						continue
					}
					if text, ok := fields["reason"].(string); ok {
						if reason := knownReason(text); reason != "" {
							result.reason = reason
						}
					}
				}
			}
		}
	}
	visit(value, 0)
	return result
}

func rpcEnvelopeFailure(raw []byte) (error, bool) {
	if len(raw) > maxErrorBody {
		return nil, false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, false
	}
	found := false
	var check func(any, int)
	check = func(item any, depth int) {
		if depth > 2 {
			return
		}
		switch v := item.(type) {
		case map[string]any:
			if child, ok := v["error"]; ok && child != nil {
				found = true
			}
		case []any:
			for i, child := range v {
				if i >= 16 {
					break
				}
				check(child, depth+1)
			}
		}
	}
	check(value, 0)
	if !found {
		return nil, false
	}
	metadata := parseRPCError(raw)
	if metadata.status == 0 && metadata.statusEnum == "" {
		return &protocolError{errors.New("unrecognized RPC error envelope")}, true
	}
	if metadata.status == 0 {
		metadata.status = httpForStatus(metadata.statusEnum)
	}
	metadata.httpStatus = http.StatusOK
	return &metadata, true
}

func httpFailure(response *http.Response, now time.Time) error {
	result := responseError{status: response.StatusCode, httpStatus: response.StatusCode, retryAfter: parseRetryAfter(response.Header.Get("Retry-After"), now)}
	data, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody+1))
	_ = response.Body.Close()
	metadata := parseRPCError(data)
	clear(data)
	result.statusEnum, result.reason = metadata.statusEnum, metadata.reason
	return &result
}

func statusForHTTP(status int) string {
	switch status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 408, 504:
		return "DEADLINE_EXCEEDED"
	case 409:
		return "ABORTED"
	case 413, 429:
		return "RESOURCE_EXHAUSTED"
	case 501:
		return "UNIMPLEMENTED"
	case 503:
		return "UNAVAILABLE"
	default:
		return "INTERNAL"
	}
}
func httpForStatus(status string) int {
	switch status {
	case "UNAUTHENTICATED":
		return 401
	case "PERMISSION_DENIED":
		return 403
	case "RESOURCE_EXHAUSTED":
		return 429
	case "INVALID_ARGUMENT", "FAILED_PRECONDITION", "OUT_OF_RANGE":
		return 400
	case "CANCELLED":
		return 499
	case "NOT_FOUND":
		return 404
	case "ALREADY_EXISTS", "ABORTED":
		return 409
	case "DEADLINE_EXCEEDED":
		return 504
	case "UNAVAILABLE":
		return 503
	case "UNIMPLEMENTED":
		return 501
	default:
		return 500
	}
}

func grpcStatus(code int) string {
	switch code {
	case 1:
		return "CANCELLED"
	case 2:
		return "UNKNOWN"
	case 3:
		return "INVALID_ARGUMENT"
	case 4:
		return "DEADLINE_EXCEEDED"
	case 5:
		return "NOT_FOUND"
	case 6:
		return "ALREADY_EXISTS"
	case 7:
		return "PERMISSION_DENIED"
	case 8:
		return "RESOURCE_EXHAUSTED"
	case 9:
		return "FAILED_PRECONDITION"
	case 10:
		return "ABORTED"
	case 11:
		return "OUT_OF_RANGE"
	case 12:
		return "UNIMPLEMENTED"
	case 13:
		return "INTERNAL"
	case 14:
		return "UNAVAILABLE"
	case 15:
		return "DATA_LOSS"
	case 16:
		return "UNAUTHENTICATED"
	}
	return ""
}

func classifyOperation(err error, stage ErrorStage, submitted, output bool, caller context.Context) *OperationError {
	result := &OperationError{Stage: stage, Submitted: submitted, OutputStarted: output, cause: err}
	effectiveHTTP := 0
	var response *responseError
	var legacy *upstreamError
	var failure *contract.Failure
	var delivery *deliveryError
	var protocol *protocolError
	var transport *transportError
	var rejected *rejectedStateError
	var network net.Error
	if errors.As(err, &response) {
		result.HTTPStatus, result.Status, result.Reason = response.status, knownStatus(response.statusEnum), knownReason(response.reason)
		effectiveHTTP = response.status
		if response.httpStatus != 0 {
			result.HTTPStatus = response.httpStatus
		}
		retry := min(max(response.retryAfter, 0), maxServerRetryAfter)
		result.RetryAfterSeconds = int64((retry + time.Second - 1) / time.Second)
	} else if errors.As(err, &legacy) {
		result.HTTPStatus = legacy.status
		effectiveHTTP = legacy.status
	}
	if effectiveHTTP == 0 && result.Status != "" {
		effectiveHTTP = httpForStatus(result.Status)
	}
	if result.Status == "" && effectiveHTTP != 0 {
		result.Status = statusForHTTP(effectiveHTTP)
	}
	if errors.As(err, &failure) {
		result.Status = knownStatus(failure.Status)
	}
	if errors.As(err, &rejected) {
		result.Status = "FAILED_PRECONDITION"
		result.Reason = knownReason(rejected.state)
	}
	switch {
	case caller != nil && caller.Err() != nil, errors.Is(err, context.Canceled):
		result.Kind = ErrorClientCancelled
	case errors.As(err, &delivery):
		result.Kind = ErrorClientWrite
		if failure != nil && (failure.Status == "RESOURCE_EXHAUSTED" || failure.Code == 413) {
			result.Kind = ErrorLocalResource
		}
	case effectiveHTTP == 401 || (effectiveHTTP == 403 && (result.Status == "UNAUTHENTICATED" || result.Reason == "ACCESS_TOKEN_EXPIRED" || result.Reason == "ACCESS_TOKEN_INVALID" || result.Reason == "CREDENTIALS_MISSING" || result.Reason == "INVALID_CREDENTIALS")):
		result.Kind = ErrorAuthentication
	case effectiveHTTP == 403:
		result.Kind = ErrorPermissionDenied
	case effectiveHTTP == 429:
		result.Kind = ErrorQuotaExhausted
	case result.Status == "DATA_LOSS":
		result.Kind = ErrorProtocol
	case effectiveHTTP >= 500 || effectiveHTTP == 408:
		result.Kind = ErrorTransient
	case effectiveHTTP >= 400:
		result.Kind = ErrorUpstreamRejected
	case effectiveHTTP >= 300:
		result.Kind = ErrorProtocol
	case failure != nil && (failure.Code == 413 || failure.Status == "RESOURCE_EXHAUSTED"):
		result.Kind = ErrorLocalResource
	case failure != nil && (failure.Code == 400 || failure.Code == 422):
		result.Kind = ErrorLocalParameter
	case failure != nil && failure.Status == "CANCELLED":
		result.Kind = ErrorClientCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network), errors.As(err, &transport):
		result.Kind = ErrorTransient
	case errors.As(err, &protocol), failure != nil && (failure.Status == "DATA_LOSS" || failure.Status == "UNIMPLEMENTED"):
		result.Kind = ErrorProtocol
	case rejected != nil:
		result.Kind = ErrorUpstreamRejected
	case failure != nil && failure.Status == "FAILED_PRECONDITION":
		result.Kind = ErrorUpstreamRejected
	default:
		result.Kind = ErrorTransient
	}
	if failure != nil {
		result.failure = failure
	}
	if result.failure == nil {
		switch result.Kind {
		case ErrorClientCancelled, ErrorClientWrite:
			result.failure = contract.Fail(499, "CANCELLED", "Business request or response delivery was cancelled").(*contract.Failure)
		case ErrorAuthentication:
			result.failure = contract.Fail(503, "UNAVAILABLE", "Business credentials need refresh").(*contract.Failure)
		case ErrorPermissionDenied:
			result.failure = contract.Fail(403, "PERMISSION_DENIED", "Business upstream denied this operation").(*contract.Failure)
		case ErrorQuotaExhausted:
			result.failure = contract.Fail(429, "RESOURCE_EXHAUSTED", "Business upstream rate limit reached").(*contract.Failure)
		case ErrorProtocol:
			result.failure = contract.Fail(502, "DATA_LOSS", "Business response is malformed or unsupported").(*contract.Failure)
		case ErrorUpstreamRejected:
			code := effectiveHTTP
			if code < 400 || code > 499 {
				code = 502
			}
			status := result.Status
			if status == "" {
				status = "FAILED_PRECONDITION"
			}
			result.failure = contract.Fail(code, status, "Business upstream rejected the request").(*contract.Failure)
		default:
			if errors.Is(err, context.DeadlineExceeded) {
				result.failure = contract.Fail(504, "DEADLINE_EXCEEDED", "Business upstream request timed out").(*contract.Failure)
			} else {
				result.failure = contract.Fail(502, "UNAVAILABLE", "Business upstream request failed").(*contract.Failure)
			}
		}
	}
	return result
}
