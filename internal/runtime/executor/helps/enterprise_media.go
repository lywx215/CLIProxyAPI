package helps

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
)

const EnterpriseMediaMaxBytes = 32 << 20

var enterpriseFileID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

// EnterpriseMediaRefs keeps upstream resource names out of public responses.
type EnterpriseMediaRefs struct{ Session, FileID, MIME string }

func (r *EnterpriseMediaRefs) Add(event enterprise.Event) error {
	switch event.Type {
	case "session":
		if r.Session != "" && r.Session != event.Session {
			return enterprise.Failure("invalid_media", "Google returned inconsistent media metadata", true)
		}
		r.Session = event.Session
	case "file":
		if !enterpriseFileID.MatchString(event.FileID) || r.FileID != "" && (r.FileID != event.FileID || r.MIME != event.MIME) {
			return enterprise.Failure("invalid_media", "Expected exactly one generated media file", true)
		}
		r.FileID, r.MIME = event.FileID, event.MIME
	}
	return nil
}

// DownloadEnterpriseMedia authenticates two explicit requests after validating the redirect.
// It never follows arbitrary redirects or retries generation when download fails.
func DownloadEnterpriseMedia(ctx context.Context, client *http.Client, prepare func(*http.Request) error, engine, kind string, refs EnterpriseMediaRefs) ([]byte, error) {
	fail := func() error {
		return enterprise.Failure("media_download_failed", "Generated media could not be downloaded; generation was not retried", true)
	}
	prefix := engine + "/sessions/"
	if !strings.HasPrefix(refs.Session, prefix) || !enterpriseFileID.MatchString(strings.TrimPrefix(refs.Session, prefix)) || !enterpriseFileID.MatchString(refs.FileID) {
		return nil, fail()
	}
	switch refs.MIME {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		if kind != "image" {
			return nil, fail()
		}
	case "video/mp4":
		if kind != "video" {
			return nil, fail()
		}
	default:
		return nil, fail()
	}
	path := "/v1alpha/" + refs.Session + ":downloadFile"
	query := url.Values{"fileId": {refs.FileID}, "alt": {"media"}}
	target := ge.Origin + path + "?" + query.Encode()
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for step := 0; step < 2; step++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fail()
		}
		req.Header.Set("X-Goog-Encode-Response-If-Executable", "base64")
		if err = prepare(req); err != nil {
			return nil, fail()
		}
		response, err := copyClient.Do(req)
		if err != nil {
			return nil, fail()
		}
		if step == 0 && response.StatusCode == http.StatusFound {
			location, errLocation := response.Location()
			_ = response.Body.Close()
			if errLocation != nil || location.Scheme != "https" || location.Host != "discoveryengine.googleapis.com" || location.User != nil || location.Fragment != "" || location.Path != "/download"+path || location.EscapedPath() != location.Path {
				return nil, fail()
			}
			q, errQuery := url.ParseQuery(location.RawQuery)
			if errQuery != nil || len(q) != 2 || len(q["fileId"]) != 1 || q.Get("fileId") != refs.FileID || len(q["alt"]) != 1 || q.Get("alt") != "media" {
				return nil, fail()
			}
			target = location.String()
			continue
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return nil, fail()
		}
		encoded := &io.LimitedReader{R: response.Body, N: int64(base64.StdEncoding.EncodedLen(EnterpriseMediaMaxBytes)) + 4097}
		data, errRead := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding.Strict(), encoded), EnterpriseMediaMaxBytes+1))
		_ = response.Body.Close()
		if errRead != nil || encoded.N <= 1 || len(data) == 0 || len(data) > EnterpriseMediaMaxBytes {
			return nil, fail()
		}
		if kind == "video" {
			if len(data) < 12 || !bytes.Equal(data[4:8], []byte("ftyp")) {
				return nil, fail()
			}
		} else if http.DetectContentType(data) != refs.MIME {
			return nil, fail()
		}
		return data, nil
	}
	return nil, fail()
}
