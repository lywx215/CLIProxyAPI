package enterpriseweb

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const googleCookieRotationURL = "https://accounts.google.com/RotateCookies"

func failureStatus(err error) int {
	var failure interface{ StatusCode() int }
	if errors.As(err, &failure) {
		return failure.StatusCode()
	}
	return 0
}

// The imported Cookie header has no scope metadata. Accept only security
// cookies observed on the Plus bootstrap that cover both Google hosts.
func mergeBootstrapCookieRotation(c Credential, res *http.Response, now time.Time) Credential {
	page, err := url.Parse(c.PageURL)
	if err != nil || page.Scheme != "https" || page.Host != "vertexaisearch.cloud.google.com" || res.StatusCode != http.StatusOK {
		return c
	}
	return mergeResponseCookies(c, res, now, "SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC")
}

// rotatePSIDTS uses the Google Accounts cookie endpoint independently of the Plus page.
func rotatePSIDTS(ctx context.Context, base *http.Client, c Credential, now time.Time) (Credential, error) {
	values, err := parseCookies(c.Cookies)
	if err != nil {
		return c, err
	}
	delete(values, "S")
	delete(values, "NID")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleCookieRotationURL, strings.NewReader(`[000,"-0000000000000000000"]`))
	if err != nil {
		return c, CredentialError(http.StatusBadGateway)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://accounts.google.com")
	req.Header.Set("User-Agent", c.Agent())
	req.Header.Set("Cookie", cookieHeader(values))
	res, err := clientCopy(base).Do(req)
	if err != nil {
		return c, CredentialError(http.StatusBadGateway)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusUnauthorized || isLoginRedirect(res) {
		return c, loginRequired()
	}
	if res.StatusCode != http.StatusOK {
		return c, cookieHTTPFailure(CredentialError(res.StatusCode), res, now)
	}
	return mergeResponseCookies(c, res, now, "__Secure-1PSIDTS", "__Secure-3PSIDTS"), nil
}

func mergeResponseCookies(c Credential, res *http.Response, now time.Time, allowed ...string) Credential {
	values, err := parseCookies(c.Cookies)
	if err != nil {
		return c
	}
	changed := false
	for _, cookie := range res.Cookies() {
		if !slices.Contains(allowed, cookie.Name) {
			continue
		}
		if !cookie.Secure || cookie.Path != "/" || !strings.EqualFold(strings.TrimPrefix(cookie.Domain, "."), "google.com") || cookie.Partitioned || cookie.Valid() != nil {
			continue
		}
		if cookie.Value == "" || cookie.MaxAge < 0 || (cookie.MaxAge == 0 && !cookie.Expires.IsZero() && !cookie.Expires.After(now)) {
			continue
		}
		if values[cookie.Name] != cookie.Value {
			values[cookie.Name] = cookie.Value
			changed = true
		}
	}
	if !changed {
		return c
	}
	updated := c
	if updated.SetCookies(cookieHeader(values)) != nil {
		return c
	}
	return updated
}

func cookieHeader(values map[string]string) string {
	pairs := make([]string, 0, len(values))
	for name, value := range values {
		pairs = append(pairs, (&http.Cookie{Name: name, Value: value}).String())
	}
	slices.Sort(pairs)
	return strings.Join(pairs, "; ")
}
