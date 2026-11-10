package enterpriseweb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

type accessState struct {
	APIKey  string           `json:"api_key"`
	Engine  string           `json:"engine"`
	Models  []string         `json:"models"`
	Catalog []AvailableModel `json:"catalog"`
}

var bootstrapData = regexp.MustCompile(`(?s)WIZ_global_data\s*=\s*({.*?});`)
var engineResource = regexp.MustCompile(`^projects/[A-Za-z0-9_-]+/locations/[a-z0-9-]+/collections/[A-Za-z0-9_-]+/engines/[A-Za-z0-9_-]+$`)
var fileIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)

func clientCopy(base *http.Client) *http.Client {
	c := *base
	c.Timeout = 0
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// Token caches verified Plus bootstrap metadata in memory; it is never an OAuth JWT.
func (r *Runtime) Token(ctx context.Context, c Credential, base *http.Client, id, key string, force bool) (string, error) {
	token, _, err := r.authenticate(ctx, &c, base, CookieSelection{ID: id}, "", cookieAuthOptions{force: force, cacheKey: key})
	return token, err
}

// Authenticate updates the caller's snapshot only after a verified cookie rotation commits.
func (r *Runtime) Authenticate(ctx context.Context, credential *Credential, base *http.Client, id, route string, force, rotate bool) (string, error) {
	token, _, err := r.authenticate(ctx, credential, base, CookieSelection{ID: id}, route, cookieAuthOptions{force: force, rotate: rotate})
	return token, err
}

// RefreshCookie verifies a selected credential for an explicit refresh.
func (r *Runtime) RefreshCookie(ctx context.Context, credential *Credential, base *http.Client, selection CookieSelection, route string, rotate bool) (string, error) {
	token, _, err := r.authenticate(ctx, credential, base, selection, route, cookieAuthOptions{force: true, rotate: rotate})
	return token, err
}

// PrepareCookie holds a shared Cookie snapshot until generation is submitted.
func (r *Runtime) PrepareCookie(ctx context.Context, credential *Credential, base *http.Client, selection CookieSelection, route, model string, rotate bool) (string, func(), error) {
	return r.authenticate(ctx, credential, base, selection, route, cookieAuthOptions{rotate: rotate, preparing: true, model: model})
}

// CheckCookie renews the Google Accounts cookies before verifying the Enterprise session.
func (r *Runtime) CheckCookie(ctx context.Context, credential *Credential, base *http.Client, selection CookieSelection, route string) error {
	_, _, err := r.authenticate(ctx, credential, base, selection, route, cookieAuthOptions{force: true, rotate: true, scheduled: true})
	return err
}

type cookieAuthOptions struct {
	force, rotate, scheduled, preparing bool
	cacheKey, model                     string
}

func (r *Runtime) authenticate(ctx context.Context, credential *Credential, base *http.Client, selection CookieSelection, route string, opts cookieAuthOptions) (_ string, releasePreparation func(), _ error) {
	gate, releaseGate := r.retainCookieGate(credential.CookieKey())
	defer func() {
		if releasePreparation == nil {
			releaseGate()
		}
	}()
	var attempt *cookieAttempt
	if opts.preparing && !isModelProbe(ctx) {
		var leaveAttempt func()
		attempt, leaveAttempt = r.joinCookieAttempt(gate, cookieTokenKey(selection.ID, *credential, route))
		defer leaveAttempt()
	}
	if err := gate.auth.Acquire(ctx, 1); err != nil {
		return "", nil, err
	}
	defer gate.auth.Release(1)
	resolve := func() error {
		if opts.scheduled && (!r.Enabled() || selection.Disabled) {
			return Fail(http.StatusServiceUnavailable, "Media input channel is disabled")
		}
		if r.resolveCookie != nil && selection.RegistrationEpoch != 0 {
			current, err := r.resolveCookie(selection, *credential)
			if err != nil {
				return err
			}
			*credential = current
		}
		if opts.preparing {
			if err := ValidateModelProbe(ctx, selection.ID, *credential, opts.model, r.EvidenceEpoch(selection.ID)); err != nil {
				return err
			}
			return ValidateModelPolicy(ctx, selection.ID, *credential, opts.model)
		}
		return nil
	}
	if err := resolve(); err != nil {
		return "", nil, err
	}
	key := opts.cacheKey
	if key == "" {
		key = cookieTokenKey(selection.ID, *credential, route)
	}
	token := ""
	if !opts.force {
		r.mu.Lock()
		if cached := r.tokens[key]; r.now().Before(cached.Expires) {
			token = cached.Token
		}
		var sharedErr error
		if token == "" && attempt != nil && attempt.failedKey == key {
			sharedErr = attempt.err
		}
		r.mu.Unlock()
		if sharedErr != nil {
			return "", nil, sharedErr
		}
	}
	if token == "" {
		var err error
		token, err = func() (string, error) {
			// The auth permit prevents new readers from entering while a writer waits.
			if opts.rotate && r.commitCookieRotation != nil && !isModelProbe(ctx) {
				if err := gate.preparations.Acquire(ctx, allCookiePreparations); err != nil {
					return "", err
				}
				defer gate.preparations.Release(allCookiePreparations)
				if err := resolve(); err != nil {
					return "", err
				}
				key = cookieTokenKey(selection.ID, *credential, route)
			}
			verified, err := r.verifyCookie(ctx, credential, base, selection.ID, key, opts)
			if err != nil && attempt != nil && ctx.Err() == nil {
				r.mu.Lock()
				attempt.failedKey, attempt.err = key, err
				r.mu.Unlock()
			}
			return verified, err
		}()
		if err != nil {
			return "", nil, err
		}
	}
	if opts.preparing {
		// Hold auth until the prepared snapshot has been reserved for submission.
		if err := gate.preparations.Acquire(ctx, 1); err != nil {
			return "", nil, err
		}
		releasePreparation = sync.OnceFunc(func() {
			gate.preparations.Release(1)
			releaseGate()
		})
	}
	return token, releasePreparation, nil
}

func cookieTokenKey(id string, credential Credential, route string) string {
	return id + ":" + credential.Fingerprint() + ":" + route
}

func (r *Runtime) verifyCookie(ctx context.Context, credential *Credential, base *http.Client, id, key string, opts cookieAuthOptions) (_ string, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c := *credential
	epoch := r.CredentialEpoch(id)
	rotate := opts.rotate && !isModelProbe(ctx)
	var degraded cookieCheckReason
	if !isModelProbe(ctx) && (opts.force || rotate) {
		defer func() {
			if ctx.Err() == nil && (opts.force || failureStatus(resultErr) == http.StatusUnauthorized) {
				r.recordCookieOutcome(id, epoch, *credential, resultErr, opts.scheduled, degraded)
			}
		}()
	}
	r.mu.Lock()
	now := r.now()
	delete(r.tokens, key)
	r.mu.Unlock()
	authCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	canSave := rotate && r.commitCookieRotation != nil
	candidate := c
	var err error
	if canSave && opts.scheduled {
		candidate, err = rotatePSIDTS(authCtx, base, c, now)
		if err != nil {
			if failureStatus(err) != http.StatusUnauthorized {
				return "", err
			}
			degraded = cookieRotationUnauthorized
		}
	}
	mergeBootstrap := canSave && (!opts.scheduled || candidate.Cookies != c.Cookies)
	access, verified, err := verifyPlus(authCtx, base, candidate, now, mergeBootstrap)
	if opts.scheduled && candidate.Cookies != c.Cookies && (failureStatus(err) == http.StatusUnauthorized || failureStatus(err) == http.StatusForbidden) {
		degraded = cookieCandidateRejected
		access, verified, err = verifyPlus(authCtx, base, c, now, false)
	}
	if err != nil {
		return "", err
	}
	candidate = verified
	encoded, _ := json.Marshal(access)
	token := string(encoded)
	if candidate.Cookies != c.Cookies {
		if opts.force {
			candidate.SetCatalog(access.Catalog, now)
		} else if catalog := c.Catalog(); len(catalog) != 0 {
			candidate.SetCatalog(catalog, c.CatalogUpdatedAt)
		}
		candidate.LastRenewedAt = now.UTC()
		committed, commitErr := r.commitCookieRotation(context.WithoutCancel(ctx), id, epoch, c, candidate)
		if commitErr != nil {
			if failureStatus(commitErr) == http.StatusConflict {
				return "", Fail(409, "Web credential changed before Cookie renewal could be saved")
			}
			return "", Fail(503, "Could not save the verified Web Cookie renewal")
		}
		*credential = committed
		key = id + ":" + committed.Fingerprint() + ":" + strings.TrimPrefix(key, id+":"+c.Fingerprint()+":")
		epoch++
	}
	r.mu.Lock()
	for k, v := range r.tokens {
		if !now.Before(v.Expires) {
			delete(r.tokens, k)
		}
	}
	if r.credentialEpochs[id] == epoch {
		r.tokens[key] = tokenState{token, now.Add(240 * time.Second), now}
	}
	r.mu.Unlock()
	return token, nil
}

func verifyPlus(ctx context.Context, base *http.Client, c Credential, now time.Time, mergeBootstrap bool) (access accessState, candidate Credential, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.PageURL, nil)
	if err != nil {
		return access, c, CredentialError(400)
	}
	req.Header.Set("Cookie", c.Cookies)
	req.Header.Set("User-Agent", c.Agent())
	res, err := clientCopy(base).Do(req)
	if err != nil {
		return access, c, CredentialError(502)
	}
	defer func() { _ = res.Body.Close() }()
	if isLoginRedirect(res) || res.StatusCode == http.StatusUnauthorized {
		return access, c, loginRequired()
	}
	if res.StatusCode != 200 {
		return access, c, cookieHTTPFailure(CredentialError(res.StatusCode), res, now)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return access, c, CredentialError(502)
	}
	match := bootstrapData.FindSubmatch(b)
	var data struct {
		APIKey string `json:"Wybhlc"`
	}
	if len(match) != 2 || json.Unmarshal(match[1], &data) != nil || data.APIKey == "" || len(data.APIKey) > 256 || strings.ContainsAny(data.APIKey, "\r\n\x00") {
		return access, c, CredentialError(502)
	}
	access = accessState{APIKey: data.APIKey}
	encoded, _ := json.Marshal(access)
	candidate = c
	if mergeBootstrap {
		candidate = mergeBootstrapCookieRotation(c, res, now)
	}
	lookup, err := lookupWidgetConfig(ctx, base, candidate, string(encoded), now)
	if candidate.Cookies != c.Cookies && failureStatus(err) == http.StatusUnauthorized {
		candidate = c
		lookup, err = lookupWidgetConfig(ctx, base, candidate, string(encoded), now)
	}
	if err != nil {
		if failureStatus(err) == http.StatusUnauthorized {
			return access, c, loginRequired()
		}
		return access, c, err
	}
	var config struct {
		Engine struct {
			Project    string `json:"projectNumber"`
			Location   string `json:"location"`
			Collection string `json:"collectionId"`
			ID         string `json:"engineId"`
		} `json:"engineResourceDetails"`
		Widget struct {
			UI struct {
				Info struct {
					Models []struct {
						ID          string `json:"modelId"`
						DisplayName string `json:"displayName"`
					} `json:"resolvedModels"`
				} `json:"modelConfigInfo"`
			} `json:"uiSettings"`
		} `json:"anonymousWidgetConfig"`
	}
	if err = ReadJSON(lookup, &config); err != nil {
		return access, c, err
	}
	access.Engine = "projects/" + config.Engine.Project + "/locations/" + config.Engine.Location + "/collections/" + config.Engine.Collection + "/engines/" + config.Engine.ID
	if !engineResource.MatchString(access.Engine) || config.Engine.Location != c.Location {
		return access, c, CredentialError(502)
	}
	for _, model := range config.Widget.UI.Info.Models {
		if model.ID != "" {
			access.Models = append(access.Models, model.ID)
			access.Catalog = append(access.Catalog, AvailableModel{ID: model.ID, DisplayName: model.DisplayName})
		}
	}
	return access, candidate, nil
}

func (c Credential) Agent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return "Mozilla/5.0"
}

func readAccess(token string) (accessState, error) {
	var access accessState
	if json.Unmarshal([]byte(token), &access) != nil || access.APIKey == "" || strings.ContainsAny(access.APIKey, "\r\n\x00") {
		return access, CredentialError(401)
	}
	return access, nil
}

func AuthorizedModel(token, model string) bool {
	access, err := readAccess(token)
	if err != nil {
		return false
	}
	for _, id := range access.Models {
		if id == model {
			return true
		}
	}
	return false
}

func ModelCatalog(token string) []AvailableModel {
	access, err := readAccess(token)
	if err != nil {
		return nil
	}
	return append([]AvailableModel(nil), access.Catalog...)
}

func setAuthentication(req *http.Request, c Credential, token string, now time.Time) error {
	access, err := readAccess(token)
	if err != nil {
		return err
	}
	values, err := parseCookies(c.Cookies)
	if err != nil {
		return err
	}
	stamp := strconv.FormatInt(now.Unix(), 10)
	var hashes []string
	for _, pair := range [][2]string{{"SAPISID", "SAPISIDHASH"}, {"__Secure-1PAPISID", "SAPISID1PHASH"}, {"__Secure-3PAPISID", "SAPISID3PHASH"}} {
		if value := values[pair[0]]; value != "" {
			digest := sha1.Sum([]byte(stamp + " " + value + " " + AuthOrigin))
			hashes = append(hashes, pair[1]+" "+stamp+"_"+hex.EncodeToString(digest[:]))
		}
	}
	req.Header.Set("Authorization", strings.Join(hashes, " "))
	req.Header.Set("Cookie", c.Cookies)
	req.Header.Set("X-Goog-Api-Key", access.APIKey)
	req.Header.Set("X-Goog-AuthUser", c.AuthUser)
	req.Header.Set("X-Goog-Api-Client", "gemini-enterprise-classic/1.0.0")
	req.Header.Set("Origin", AuthOrigin)
	req.Header.Set("Referer", AuthOrigin+"/")
	req.Header.Set("User-Agent", c.Agent())
	return nil
}

func send(ctx context.Context, base *http.Client, c Credential, token, target string, body io.Reader, size int64, headers http.Header, now time.Time) (*http.Response, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme+"://"+u.Host != c.APIOrigin() || u.User != nil || u.Fragment != "" {
		return nil, Fail(502, "Invalid media upstream destination")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, Fail(400, "Invalid media request")
	}
	req.ContentLength = size
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	if err = setAuthentication(req, c, token, now); err != nil {
		return nil, err
	}
	response, err := clientCopy(base).Do(req)
	if err != nil {
		return nil, Fail(502, "Media upstream connection failed")
	}
	if response.StatusCode != 200 {
		_ = response.Body.Close()
		err := Fail(response.StatusCode, "Media upstream rejected the request")
		if strings.HasSuffix(u.Path, "/lookupWidgetConfig") {
			if isLoginRedirect(response) {
				return nil, loginRequired()
			}
			err = cookieHTTPFailure(err, response, now)
		}
		return nil, err
	}
	return response, nil
}

func Post(ctx context.Context, base *http.Client, c Credential, token, action string, body io.Reader, size int64) (*http.Response, error) {
	switch action {
	case "widgetCreateSession", "widgetStreamAssist", "lookupWidgetConfig":
	default:
		return nil, Fail(400, "Unsupported media operation")
	}
	return send(ctx, base, c, token, c.APIBase()+action, body, size, http.Header{"Content-Type": []string{"application/json"}}, time.Now())
}

func postJSONAt(ctx context.Context, base *http.Client, c Credential, token, action string, body any, now time.Time) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, Fail(400, "Invalid media request")
	}
	return send(ctx, base, c, token, c.APIBase()+action, bytes.NewReader(b), int64(len(b)), http.Header{"Content-Type": []string{"application/json"}}, now)
}

func lookupWidgetConfig(ctx context.Context, base *http.Client, c Credential, token string, now time.Time) (*http.Response, error) {
	return postJSONAt(ctx, base, c, token, "lookupWidgetConfig", map[string]string{
		"widgetConfigId": c.ConfigID,
		"modelInfoView":  "END_USER_WEB",
	}, now)
}

func PostJSON(ctx context.Context, base *http.Client, c Credential, token, action string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, Fail(400, "Invalid media request")
	}
	return Post(ctx, base, c, token, action, bytes.NewReader(b), int64(len(b)))
}

func ReadJSON(res *http.Response, value any) error {
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 || json.Unmarshal(b, value) != nil {
		return Fail(502, "Invalid media upstream response")
	}
	return nil
}

func NormalizeSession(token, session string) (string, error) {
	access, err := readAccess(token)
	if err != nil {
		return "", err
	}
	if !engineResource.MatchString(access.Engine) {
		return "", Fail(502, "Invalid media engine resource")
	}
	start := strings.Index(access.Engine, "/collections/")
	relativeEngine := access.Engine[start+1:]
	if strings.HasPrefix(session, access.Engine+"/sessions/") {
		session = relativeEngine + strings.TrimPrefix(session, access.Engine)
	}
	prefix := relativeEngine + "/sessions/"
	if !strings.HasPrefix(session, prefix) || !component.MatchString(strings.TrimPrefix(session, prefix)) {
		return "", Fail(502, "Invalid media session response")
	}
	return session, nil
}

// Upload streams decoded bytes through the Plus resumable upload protocol.
func Upload(ctx context.Context, base *http.Client, c Credential, token, session string, a contract.Attachment) (string, error) {
	relative, err := NormalizeSession(token, session)
	if err != nil {
		return "", err
	}
	access, _ := readAccess(token)
	fullSession := access.Engine + "/sessions/" + relative[strings.LastIndex(relative, "/")+1:]
	target := c.APIOrigin() + "/upload/v1alpha/" + fullSession + ":uploadFile"
	headers := http.Header{}
	headers.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	headers.Set("X-Goog-Upload-Protocol", "resumable")
	headers.Set("X-Goog-Upload-Command", "start")
	headers.Set("X-Goog-Upload-File-Name", a.Filename)
	headers.Set("X-Goog-Upload-Header-Content-Length", strconv.Itoa(len(a.Data)))
	headers.Set("X-Goog-Upload-Header-Content-Type", a.MIMEType)
	res, err := send(ctx, base, c, token, target, strings.NewReader(""), 0, headers, time.Now())
	if err != nil {
		return "", err
	}
	uri := res.Header.Get("X-Goog-Upload-URL")
	_ = res.Body.Close()
	u, err := url.Parse(uri)
	if err != nil || u.Scheme+"://"+u.Host != c.APIOrigin() || u.Path != "/upload/v1alpha/"+fullSession+":uploadFile" || u.User != nil || u.Fragment != "" {
		return "", Fail(502, "Invalid media upload destination")
	}
	headers = http.Header{}
	headers.Set("Content-Type", a.MIMEType)
	headers.Set("X-Goog-Upload-Command", "upload, finalize")
	headers.Set("X-Goog-Upload-Offset", "0")
	headers.Set("X-Goog-Upload-File-Name", a.Filename)
	res, err = send(ctx, base, c, token, uri, bytes.NewReader(a.Data), int64(len(a.Data)), headers, time.Now())
	if err != nil {
		return "", err
	}
	var file struct {
		ID string `json:"fileId"`
	}
	if err = ReadJSON(res, &file); err != nil {
		return "", err
	}
	if !fileIdentifier.MatchString(file.ID) {
		return "", Fail(502, "Invalid media file response")
	}
	return file.ID, nil
}
