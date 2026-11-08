package antigravityenterprise

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	ag "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	"golang.org/x/oauth2"
)

func OAuth(redirect string) *oauth2.Config {
	return &oauth2.Config{ClientID: ag.ClientID, ClientSecret: ag.ClientSecret, RedirectURL: redirect, Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token"}, Scopes: []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile", "https://www.googleapis.com/auth/cclog", "https://www.googleapis.com/auth/experimentsandconfigs"}}
}
func SafeClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copyClient
}

type Identity struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Verified bool   `json:"verified_email"`
}

func UserInfo(ctx context.Context, client *http.Client, token string) (Identity, error) {
	var result Identity
	err := get(ctx, client, "https://www.googleapis.com/oauth2/v2/userinfo", token, "", &result)
	if err == nil && (result.ID == "" || result.Email == "" || !result.Verified) {
		err = fmt.Errorf("%s: unverified Google identity", Provider)
	}
	return result, err
}
func FetchLicenses(ctx context.Context, client *http.Client, token, quotaProject string) ([]License, []string, error) {
	var result struct {
		Licenses    []License `json:"licenses"`
		Unreachable []string  `json:"unreachable"`
	}
	err := get(ctx, client, Origin("global")+"/v1beta:fetchLicenses", token, quotaProject, &result)
	return result.Licenses, result.Unreachable, err
}
func endpoint(l License, action string) string {
	return Origin(l.Location) + "/v1beta/" + l.Parent() + ":" + action + "?" + url.Values{"entitlement.userTier": {l.UserTier}}.Encode()
}
func FetchConfig(ctx context.Context, client *http.Client, token string, l License) (Config, error) {
	var c Config
	if err := l.Validate(); err != nil {
		return c, err
	}
	err := get(ctx, client, endpoint(l, "fetchConfig"), token, l.ProjectID, &c)
	return c, err
}
func FetchQuota(ctx context.Context, client *http.Client, token string, l License) (Quota, error) {
	var q Quota
	if err := l.Validate(); err != nil {
		return q, err
	}
	err := get(ctx, client, endpoint(l, "fetchQuotaStatus"), token, l.ProjectID, &q)
	if err == nil {
		q.ObservedAt = time.Now().UTC()
	}
	return q, err
}
func get(ctx context.Context, client *http.Client, target, token, project string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	SetHeaders(req, token, project)
	response, err := SafeClient(client).Do(req)
	if err != nil {
		return &Error{HTTP: 502, Code: "metadata_transport"}
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return &Error{HTTP: 502, Code: "metadata_read"}
	}
	if len(data) > 4<<20 {
		return &Error{HTTP: 502, Code: "metadata_response_too_large"}
	}
	if response.StatusCode != 200 {
		return DecodeError(response.StatusCode, response.Header, data, false)
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return DecodeError(response.StatusCode, response.Header, data, false)
	}
	if json.Unmarshal(data, out) != nil {
		return &Error{HTTP: 502, Code: "metadata_decode"}
	}
	return nil
}
func SetHeaders(req *http.Request, token, project string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if project != "" {
		req.Header.Set("X-Goog-User-Project", project)
	}
}
func DecodeError(status int, headers http.Header, data []byte, started bool) *Error {
	e := &Error{HTTP: status, Code: "upstream_error", Started: started}
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Details []struct {
				Type       string `json:"@type"`
				Reason     string `json:"reason"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil {
		if status >= 200 && status < 300 {
			e.HTTP = http.StatusBadGateway
			if envelope.Error.Code >= 400 && envelope.Error.Code <= 599 {
				e.HTTP = envelope.Error.Code
			} else {
				statuses := map[string]int{"INVALID_ARGUMENT": 400, "FAILED_PRECONDITION": 400, "OUT_OF_RANGE": 400, "NOT_FOUND": 404, "ALREADY_EXISTS": 409, "ABORTED": 409, "PERMISSION_DENIED": 403, "UNAUTHENTICATED": 401, "RESOURCE_EXHAUSTED": 429, "CANCELLED": 499, "DEADLINE_EXCEEDED": 504, "UNIMPLEMENTED": 501, "INTERNAL": 500, "UNAVAILABLE": 503, "DATA_LOSS": 502}
				if mapped := statuses[envelope.Error.Status]; mapped != 0 {
					e.HTTP = mapped
				} else {
					codes := map[int]int{1: 499, 3: 400, 4: 504, 5: 404, 6: 409, 7: 403, 8: 429, 9: 400, 10: 409, 11: 400, 12: 501, 13: 500, 14: 503, 15: 502, 16: 401}
					if mapped = codes[envelope.Error.Code]; mapped != 0 {
						e.HTTP = mapped
					}
				}
			}
		}
		e.RPCStatus = envelope.Error.Status
		for _, d := range envelope.Error.Details {
			if strings.HasSuffix(d.Type, "google.rpc.RetryInfo") {
				if delay, err := time.ParseDuration(d.RetryDelay); err == nil && delay > 0 {
					e.RetryAt = time.Now().Add(delay)
					e.RetryHint = true
				}
			}
			if strings.HasSuffix(d.Type, "google.rpc.ErrorInfo") {
				switch d.Reason {
				case "MODEL_NOT_ENABLED", "MODEL_NOT_FOUND", "MODEL_NOT_ALLOWED_BY_ADMIN":
					e.Code = "model_not_enabled"
					e.Scope = "model"
				case "IAM_PERMISSION_DENIED", "SERVICE_DISABLED":
					e.Code = "permission_denied"
					e.Scope = "binding"
				}
			}
		}
	}
	if headers != nil {
		if seconds, err := strconv.ParseInt(headers.Get("Retry-After"), 10, 64); err == nil && seconds > 0 && seconds < 31536000 {
			at := time.Now().Add(time.Duration(seconds) * time.Second)
			if at.After(e.RetryAt) {
				e.RetryAt = at
			}
			e.RetryHint = true
		} else if at, errDate := http.ParseTime(headers.Get("Retry-After")); errDate == nil && at.After(e.RetryAt) {
			e.RetryAt = at
			e.RetryHint = true
		}
	}
	if e.HTTP == 429 && e.RetryAt.IsZero() {
		e.RetryAt = time.Now().Add(10 * time.Second)
	}
	if e.HTTP == 403 && e.Code == "upstream_error" {
		e.Code = "permission_denied"
		e.Scope = "binding"
	}
	return e
}
