package enterpriseweb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

const Provider = "gemini-enterprise-web"
const AuthOrigin = "https://vertexaisearch.cloud.google.com"
const MaxBodyBytes int64 = 68 << 20

type AvailableModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}
type Credential struct {
	Cookies            string           `json:"cookies"`
	Location           string           `json:"location"`
	AuthUser           string           `json:"auth_user"`
	ConfigID           string           `json:"config_id"`
	UserAgent          string           `json:"user_agent,omitempty"`
	PageURL            string           `json:"page_url"`
	ModelPolicy        string           `json:"model_policy,omitempty"`
	AvailableModels    []AvailableModel `json:"available_models,omitempty"`
	CatalogFingerprint string           `json:"catalog_fingerprint,omitempty"`
	CatalogUpdatedAt   time.Time        `json:"catalog_updated_at,omitempty"`
	LastRenewedAt      time.Time        `json:"cookie_last_renewed_at,omitzero"`
	Revision           uint64           `json:"revision"`
}

var component = regexp.MustCompile(`^[A-Za-z0-9_-]{1,1000}$`)
var accountIndex = regexp.MustCompile(`^[0-9]{1,3}$`)
var regionName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

var cookieNames = map[string]bool{
	"SID": true, "HSID": true, "SSID": true, "APISID": true, "SAPISID": true,
	"__Secure-1PSID": true, "__Secure-3PSID": true, "__Secure-1PAPISID": true, "__Secure-3PAPISID": true,
	"SIDCC": true, "__Secure-1PSIDCC": true, "__Secure-3PSIDCC": true,
	"__Secure-1PSIDTS": true, "__Secure-3PSIDTS": true, "NID": true, "S": true,
}

func parseCookies(raw string) (map[string]string, error) {
	if len(raw) > 40000 || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, Fail(400, "Invalid Google Cookie header")
	}
	for _, b := range raw {
		if b < 32 || b > 126 {
			return nil, Fail(400, "Invalid Google Cookie header")
		}
	}
	r := &http.Request{Header: make(http.Header)}
	r.Header.Set("Cookie", raw)
	seen := map[string]string{}
	for _, v := range r.Cookies() {
		if !cookieNames[v.Name] {
			continue
		}
		if old, ok := seen[v.Name]; ok && old != v.Value {
			return nil, Fail(400, "Conflicting cookie values")
		}
		if err := v.Valid(); err != nil {
			return nil, Fail(400, "Invalid Google session cookie")
		}
		seen[v.Name] = v.Value
	}
	if (seen["SID"] == "" && seen["__Secure-1PSID"] == "" && seen["__Secure-3PSID"] == "") ||
		(seen["SAPISID"] == "" && seen["__Secure-1PAPISID"] == "" && seen["__Secure-3PAPISID"] == "") {
		return nil, Fail(400, "Google SID and SAPISID family cookies are required for Enterprise Plus")
	}
	return seen, nil
}

func (c *Credential) SetCookies(raw string) error {
	seen, err := parseCookies(raw)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]string, 0, len(names))
	for _, name := range names {
		values = append(values, (&http.Cookie{Name: name, Value: seen[name]}).String())
	}
	c.Cookies = strings.Join(values, "; ")
	return nil
}
func (c *Credential) SetURL(raw string) error {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.Scheme != "https" || u.Host != "vertexaisearch.cloud.google.com" || u.User != nil || u.Fragment != "" {
		return Fail(400, "A valid HTTPS Gemini Enterprise Plus page address is required")
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return Fail(400, "Invalid Plus page query")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	user := "0"
	if len(parts) >= 2 && parts[0] == "u" {
		user = parts[1]
		parts = parts[2:]
	}
	if len(q["authuser"]) > 1 {
		return Fail(400, "Invalid Google account index")
	}
	if q.Has("authuser") {
		user = q.Get("authuser")
	}
	if !accountIndex.MatchString(user) {
		return Fail(400, "Invalid Google account index")
	}
	location := "global"
	if len(parts) > 0 && parts[0] != "home" {
		location = parts[0]
		parts = parts[1:]
	}
	if !regionName.MatchString(location) || len(parts) < 3 || parts[0] != "home" || parts[1] != "cid" || !component.MatchString(parts[2]) {
		return Fail(400, "The Plus page address must contain a region, home and one valid cid")
	}
	if len(parts) != 3 && (len(parts) != 6 || parts[3] != "r" || parts[4] != "session" || !component.MatchString(parts[5])) {
		return Fail(400, "Invalid Plus page path")
	}
	c.ConfigID, c.Location, c.AuthUser = parts[2], location, user
	prefix := "/"
	if user != "0" {
		prefix += "u/" + user + "/"
	}
	if location != "global" {
		prefix += location + "/"
	}
	c.PageURL = AuthOrigin + prefix + "home/cid/" + c.ConfigID
	return nil
}
func (c Credential) Validate() error {
	if c.ModelPolicy != "" && !ValidModelPolicy(c.ModelPolicy) {
		return Fail(400, "Web model_policy must be argon_only or exclude_argon")
	}
	var canonical Credential
	if canonical.SetURL(c.PageURL) != nil || canonical.ConfigID != c.ConfigID || canonical.Location != c.Location || canonical.AuthUser != c.AuthUser {
		return Fail(400, "Cookie credential or page identifiers are incomplete")
	}
	if _, err := parseCookies(c.Cookies); err != nil {
		return err
	}
	if len(c.UserAgent) > 1024 || strings.ContainsAny(c.UserAgent, "\r\n\x00") {
		return Fail(400, "Invalid user agent")
	}
	if len(c.AvailableModels) > 256 {
		return Fail(400, "Too many available models")
	}
	seen := map[string]bool{}
	for _, m := range c.AvailableModels {
		if strings.EqualFold(m.ID, "auto") || strings.EqualFold(m.ID, "default") || m.ID == "" || len(m.ID) > 160 || len(m.DisplayName) > 200 || strings.ContainsAny(m.ID, "\r\n\t ()\\") || strings.ContainsAny(m.DisplayName, "\r\n\x00") || seen[m.ID] {
			return Fail(400, "Invalid available model catalog")
		}
		seen[m.ID] = true
	}
	return nil
}
func CredentialFrom(m map[string]any) (Credential, error) {
	var c Credential
	b, e := json.Marshal(m["enterprise_web"])
	if e != nil {
		return c, Fail(400, "Invalid cookie credential")
	}
	if json.Unmarshal(b, &c) != nil {
		return c, Fail(400, "Invalid cookie credential")
	}
	return c, c.Validate()
}
func (c Credential) Fingerprint() string {
	b, _ := json.Marshal([]string{c.Cookies, c.Location, c.AuthUser, c.ConfigID, c.UserAgent})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func (c Credential) MetadataValue() map[string]any {
	b, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
func (c Credential) Summary() map[string]any {
	summary := map[string]any{"page_url": c.PageURL, "location": c.Location, "auth_user": c.AuthUser, "user_agent": c.UserAgent, "model_policy": c.Policy(), "available_models": c.Catalog(), "catalog_updated_at": c.CatalogUpdatedAt, "revision": c.Revision, "cookie_present": c.Cookies != ""}
	if !c.LastRenewedAt.IsZero() {
		summary["cookie_last_renewed_at"] = c.LastRenewedAt
	}
	return summary
}
func (c Credential) Catalog() []AvailableModel {
	if c.CatalogFingerprint != c.Fingerprint() {
		return []AvailableModel{}
	}
	return append([]AvailableModel{}, c.AvailableModels...)
}

func (c *Credential) SetCatalog(models []AvailableModel, checkedAt time.Time) {
	c.AvailableModels = append([]AvailableModel{}, models...)
	c.CatalogFingerprint = c.Fingerprint()
	c.CatalogUpdatedAt = checkedAt.UTC()
}

func (c *Credential) ClearCatalog() {
	c.AvailableModels = nil
	c.CatalogFingerprint = ""
	c.CatalogUpdatedAt = time.Time{}
}

type Failure struct {
	Code             int
	Message          string
	clientMessage    string
	Reason, Param    string
	Limit            *contract.InputLimit
	AttachmentReason contract.AttachmentReason
}

func (e *Failure) Error() string         { return e.Message }
func (e *Failure) StatusCode() int       { return e.Code }
func (e *Failure) IsRequestScoped() bool { return true }
func Fail(code int, message string) error {
	return &Failure{Code: code, Message: message, clientMessage: message}
}

// PublicErrorDetail exposes only locally constructed messages, never upstream text.
func PublicErrorDetail(err error, status int) (string, string) {
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != status || failure.clientMessage == "" {
		return "", ""
	}
	if failure.Reason != "" {
		return failure.clientMessage, failure.Reason
	}
	return failure.clientMessage, "media_input_error"
}

func InputFailure(code int, reason, param, message string) error {
	return &Failure{Code: code, Message: message, clientMessage: message, Reason: reason, Param: param}
}

func InputFailureFrom(failure *contract.Failure) error {
	return &Failure{
		Code: failure.Code, Message: failure.Message, clientMessage: failure.Message,
		Reason: failure.Reason, Param: failure.Param, Limit: failure.Limit,
		AttachmentReason: failure.AttachmentReason,
	}
}

func PublicErrorParam(err error, status int) string {
	var failure *Failure
	if errors.As(err, &failure) && failure.Code == status && failure.clientMessage != "" {
		return failure.Param
	}
	return ""
}
func CredentialError(code int) error {
	return Fail(code, fmt.Sprintf("Media authentication failed (HTTP %d); update the session cookie if necessary", code))
}

// CookieKey coordinates authentication and rotation across copies of a session.
func (c Credential) CookieKey() string {
	values, _ := parseCookies(c.Cookies)
	identity := values["SID"]
	if identity == "" {
		identity = values["__Secure-1PSID"]
	}
	if identity == "" {
		identity = values["__Secure-3PSID"]
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func (c Credential) APIOrigin() string {
	if c.Location == "global" {
		return "https://discoveryengine.clients6.google.com"
	}
	return "https://" + c.Location + "-discoveryengine.clients6.google.com"
}

func (c Credential) APIBase() string { return c.APIOrigin() + "/v1alpha/locations/" + c.Location + "/" }
