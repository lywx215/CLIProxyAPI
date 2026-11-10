package enterpriseweb

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

func enabledRuntime() *Runtime {
	r := NewRuntime()
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true})
	return r
}
func credentialFixture(t *testing.T) Credential {
	t.Helper()
	c := Credential{AvailableModels: []AvailableModel{{ID: "observed-web-id", DisplayName: "public-model"}}, Revision: 1}
	if err := c.SetURL("https://vertexaisearch.cloud.google.com/ca/home/cid/test-config"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCookies("SID=synthetic-cookie; __Host-C_OSES=synthetic-host; SAPISID=synthetic-signing"); err != nil {
		t.Fatal(err)
	}
	c.SetCatalog(c.AvailableModels, time.Now())
	return c
}
func code(err error) int {
	if e, ok := err.(interface{ StatusCode() int }); ok {
		return e.StatusCode()
	}
	return 0
}

func TestAdmissionDisableReopenAndLowerLimits(t *testing.T) {
	r := enabledRuntime()
	ctx, first, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if ExistingLease(ctx) != first {
		t.Fatal("lease missing from context")
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: false})
	if _, _, err = r.Acquire(context.Background(), 1024); code(err) != 503 {
		t.Fatalf("disabled: %v", err)
	}
	if r.Status()["draining"] != true || r.Status()["active_requests"] != 1 {
		t.Fatal(r.Status())
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, MaxConcurrent: 1, MaxInflightBytes: 64 << 20})
	if _, _, err = r.Acquire(context.Background(), 1024); code(err) != 429 {
		t.Fatalf("reopen lost old lease: %v", err)
	}
	first.Release()
	first.Release()
	_, next, err := r.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	next.Release()
	if r.Status()["active_requests"] != 0 || r.Status()["inflight_bytes"] != int64(0) {
		t.Fatal(r.Status())
	}
}

func TestGlobalAdmissionRace(t *testing.T) {
	r := enabledRuntime()
	var wg sync.WaitGroup
	start := make(chan struct{})
	finish := make(chan struct{})
	results := make(chan bool, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, lease, err := r.Acquire(context.Background(), 1024)
			results <- err == nil
			if err == nil {
				<-finish
				lease.Release()
			}
		}()
	}
	close(start)
	success := 0
	for i := 0; i < 32; i++ {
		if <-results {
			success++
		}
	}
	if success != 2 {
		t.Fatalf("admitted %d", success)
	}
	close(finish)
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Acquire(ctx, 1); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestMemoryReservationAndUnknownLength(t *testing.T) {
	r := enabledRuntime()
	_, large, err := r.Acquire(context.Background(), MaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.Acquire(context.Background(), 10<<20); code(err) != 429 {
		t.Fatalf("budget oversubscription: %v", err)
	}
	r.Configure(config.GeminiEnterpriseWebConfig{Enabled: true, MaxInflightBytes: 64 << 20})
	if large.Budget() < 200<<20 {
		t.Fatal("old lease snapshot changed")
	}
	large.Release()
	if _, _, err = r.Acquire(context.Background(), -1); code(err) != 429 {
		t.Fatalf("unknown must reserve maximum: %v", err)
	}
	if r.Status()["rejected_memory"].(uint64) != 2 {
		t.Fatal(r.Status())
	}
}

type fragmentReader struct {
	io.Reader
	size int
}

func (f fragmentReader) Read(b []byte) (int, error) { return f.Reader.Read(b[:min(len(b), f.size)]) }
func TestIncrementalAdmissionAndPrefixSpool(t *testing.T) {
	for _, prefix := range []int{0, 70000} {
		for _, length := range []int64{-1, 100000} {
			payload := `{"contents":[{"parts":[{"text":"` + strings.Repeat("x", prefix) + `"},{"inli\u006eeData":{"mimeType":"text/plain","data":"YQ=="}}]}]}`
			declared := length
			if declared >= 0 {
				declared = int64(len(payload))
			}
			r := enabledRuntime()
			body, ctx, lease, err := ReadBody(context.Background(), fragmentReader{strings.NewReader(payload), 7}, declared, r)
			if err != nil || lease == nil || !HasMedia(body) || !bytes.Equal(body, []byte(payload)) || ExistingLease(ctx) != lease {
				t.Fatalf("prefix=%d length=%d err=%v bytes=%d", prefix, length, err, len(body))
			}
			lease.Release()
		}
	}
	r := NewRuntime()
	body, _, lease, err := ReadBody(context.Background(), strings.NewReader(`{"contents":[{"parts":[{"text":"inlineData: not media"}]}]}`), -1, r)
	if err != nil || lease != nil || HasMedia(body) {
		t.Fatalf("text incorrectly admitted: %v", err)
	}
	media := `{"contents":[{"parts":[{"inlineData":{"mimeType":"text/plain","data":"` + strings.Repeat("A", 100000) + `"}}]}]}`
	reader := strings.NewReader(media)
	if _, _, _, err = ReadBody(context.Background(), reader, -1, r); code(err) != 503 || reader.Len() < 90000 {
		t.Fatal("disabled channel buffered the attachment")
	}
}

func TestReadBodyPreservesWebAttachmentsAndDropsPlainPlaceholders(t *testing.T) {
	for _, tc := range []struct {
		name, parts, wantParts string
		media                  bool
	}{
		{"empty and escaped keys",
			`{"inline_data":{"mime_type":"image/png","da\u0074a": ""}},{"text":"ask"},{"inli\u006eeData":{"data":"","mimeType":"image/png"}}`,
			`{"text":"ask"}`, false},
		{"text and placeholder in one part",
			`{"text":"ask","inlineData":{"mimeType":"image/png","data":""}}`,
			`{"text":"ask"}`, false},
		{"empty with real media",
			`{"inlineData":{"data":""}},{"inlineData":{"mimeType":"text/plain","data":"eA=="}},{"inline_data":{"data":""}}`,
			`{"inlineData":{"data":""}},{"inlineData":{"mimeType":"text/plain","data":"eA=="}},{"inline_data":{"data":""}}`, true},
		{"file reference",
			`{"inlineData":{"data":""}},{"fileData":{"fileUri":"files/fixture","mimeType":"image/png"}}`,
			`{"inlineData":{"data":""}},{"fileData":{"fileUri":"files/fixture","mimeType":"image/png"}}`, true},
		{"null data",
			`{"inlineData":{"mimeType":"image/png","data":null}}`,
			`{"inlineData":{"mimeType":"image/png","data":null}}`, true},
		{"missing data",
			`{"inlineData":{"mimeType":"image/png"}}`,
			`{"inlineData":{"mimeType":"image/png"}}`, true},
		{"nonempty whitespace",
			`{"inlineData":{"mimeType":"image/png","data":" "}}`,
			`{"inlineData":{"mimeType":"image/png","data":" "}}`, true},
		{"duplicate with real data",
			`{"inlineData":{"mimeType":"image/png","data":"","data":"eA=="}}`,
			`{"inlineData":{"mimeType":"image/png","data":"","data":"eA=="}}`, true},
		{"empty file placeholders",
			`{"fileData":{}},{"file_data":null},{"text":"ask"},{"fileData":{"mimeType":"image/png"}},{"file\u005fdata":{"file_\u0075ri":""}},{"file_data":{"file_uri":null}}`,
			`{"text":"ask"}`, false},
		{"empty file in text part",
			`{"text":"ask","file_data":null}`,
			`{"text":"ask"}`, false},
		{"empty file and inline attachment in same part",
			`{"fileData":{},"inlineData":{"mimeType":"text/plain","data":"eA=="}}`,
			`{"fileData":{},"inlineData":{"mimeType":"text/plain","data":"eA=="}}`, true},
		{"empty file placeholders around a reference",
			`{"fileData":null},{"file_data":{"file_uri":"files/biz-fixture","mime_type":"image/png"}},{"fileData":{"mimeType":"image/png"}}`,
			`{"fileData":null},{"file_data":{"file_uri":"files/biz-fixture","mime_type":"image/png"}},{"fileData":{"mimeType":"image/png"}}`, true},
		{"conflicting URI aliases",
			`{"fileData":{"fileUri":"","file_uri":"files/biz-fixture","mimeType":"image/png"}}`,
			`{"fileData":{"fileUri":"","file_uri":"files/biz-fixture","mimeType":"image/png"}}`, true},
		{"non-string URI",
			`{"fileData":{"fileUri":42}}`,
			`{"fileData":{"fileUri":42}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const prefix = `{"contents":[{"role":"user","parts":[`
			const suffix = `]}],"systemInstruction":{"parts":[{"text":"keep original fields"}]}}`
			payload, want := prefix+tc.parts+suffix, prefix+tc.wantParts+suffix
			for _, fragment := range []int{1, 7, 4096} {
				rt := NewRuntime()
				if tc.media {
					rt = enabledRuntime()
				}
				body, ctx, lease, err := ReadBody(t.Context(), fragmentReader{strings.NewReader(payload), fragment}, int64(len(payload)), rt)
				if lease != nil {
					lease.Release()
				}
				if err != nil || (lease != nil) != tc.media || HasMediaInContext(WithBody(ctx, body), body) != tc.media || string(body) != want {
					t.Fatalf("fragment %d: body=%s, lease=%v, err=%v", fragment, body, lease != nil, err)
				}
			}
		})
	}
}

func TestReadBodyRejectsEmptyAttachmentTurn(t *testing.T) {
	for _, tc := range []struct{ part, wantCode string }{
		{`{"inlineData":{"mimeType":"image/png","data":""}}`, "empty_inline_data"},
		{`{"fileData":null}`, "empty_file_data"},
	} {
		t.Run(tc.wantCode, func(t *testing.T) {
			payload := `{"contents":[{"role":"user","parts":[{"text":"earlier question"}]},{"role":"model","parts":[{"text":"earlier answer"}]},{"role":"user","parts":[{"text":"  "},` + tc.part + `]}]}`
			_, _, lease, err := ReadBody(t.Context(), strings.NewReader(payload), int64(len(payload)), NewRuntime())
			message, reason := PublicErrorDetail(err, http.StatusBadRequest)
			if lease != nil || code(err) != http.StatusBadRequest || reason != tc.wantCode || message == "" || PublicErrorParam(err, http.StatusBadRequest) != "contents[2].parts" {
				t.Fatalf("empty current input was not rejected: %v", err)
			}
		})
	}
}

func TestCredentialValidationAndRedaction(t *testing.T) {
	c := credentialFixture(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	summary, _ := json.Marshal(c.Summary())
	if bytes.Contains(summary, []byte("synthetic-cookie")) || bytes.Contains(summary, []byte("cookies")) {
		t.Fatal("secret in summary")
	}
	for _, url := range []string{"http://business.gemini.google/cid/test?csesidx=1", "https://evil.example/cid/test?csesidx=1", "https://business.gemini.google/cid/test?csesidx=1&csesidx=2", "https://user@business.gemini.google/cid/test?csesidx=1"} {
		if c.SetURL(url) == nil {
			t.Fatalf("accepted %s", url)
		}
	}
	if c.SetCookies("SID=one; SID=two; SAPISID=synthetic-signing") == nil {
		t.Fatal("ambiguous cookies accepted")
	}
	c = credentialFixture(t)
	c.AvailableModels = append(c.AvailableModels, c.AvailableModels[0])
	if c.Validate() == nil {
		t.Fatal("duplicate mapping accepted")
	}
}

type syntheticTransport func(*http.Request) (*http.Response, error)

func (f syntheticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const syntheticPlusConfig = `{"engineResourceDetails":{"projectNumber":"123","location":"ca","collectionId":"default_collection","engineId":"synthetic"},"anonymousWidgetConfig":{"uiSettings":{"modelConfigInfo":{"resolvedModels":[{"modelId":"observed-web-id"}]}}}}`

func TestPlusAuthenticationCacheAndFixedOrigins(t *testing.T) {
	r := enabledRuntime()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	c := credentialFixture(t)
	calls := 0
	client := &http.Client{Timeout: time.Second, Transport: syntheticTransport(func(req *http.Request) (*http.Response, error) {
		reply := `{}`
		if req.Method == http.MethodGet {
			calls++
			if req.URL.Host != "vertexaisearch.cloud.google.com" || req.Header.Get("Cookie") == "" || req.Header.Get("Authorization") != "" {
				t.Fatal("incorrect bootstrap boundary")
			}
			if _, ok := req.Context().Deadline(); !ok {
				t.Fatal("missing acquisition deadline")
			}
			reply = `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`
		} else {
			if req.URL.Host != "ca-discoveryengine.clients6.google.com" || req.Header.Get("Cookie") == "" || !strings.HasPrefix(req.Header.Get("Authorization"), "SAPISIDHASH ") || req.Header.Get("X-Goog-Api-Key") != "synthetic-api-key" {
				t.Fatal("incorrect Plus authentication")
			}
			_, timed := req.Context().Deadline()
			if strings.HasSuffix(req.URL.Path, "lookupWidgetConfig") {
				if !timed {
					t.Fatal("missing lookup acquisition deadline")
				}
				reply = syntheticPlusConfig
			} else if timed {
				t.Fatal("generation has acquisition deadline")
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)), Header: http.Header{}}, nil
	})}
	token, err := r.Token(context.Background(), c, client, "account", "account:"+c.Fingerprint()+":", false)
	if err != nil || !AuthorizedModel(token, "observed-web-id") || AuthorizedModel(token, "Auto") {
		t.Fatal("invalid authenticated model snapshot", err)
	}
	if _, err = r.Token(context.Background(), c, client, "account", "account:"+c.Fingerprint()+":", false); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	res, err := PostJSON(context.Background(), client, c, token, "widgetCreateSession", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	now = now.Add(241 * time.Second)
	if _, err = r.Token(context.Background(), c, client, "account", "account:"+c.Fingerprint()+":", false); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
	r.ForgetCredential("account")
	if len(r.tokens) != 0 {
		t.Fatal("authentication cache was not invalidated")
	}
}

func TestPlusRawResumableUploadAndOriginBinding(t *testing.T) {
	c := credentialFixture(t)
	raw := bytes.Repeat([]byte("xyz"), 15000)
	access, _ := json.Marshal(accessState{APIKey: "synthetic-key", Engine: "projects/123/locations/ca/collections/default_collection/engines/synthetic"})
	session := "collections/default_collection/engines/synthetic/sessions/test"
	for _, untrusted := range []bool{false, true} {
		calls := 0
		client := &http.Client{Transport: syntheticTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			header := http.Header{}
			reply := `{}`
			if calls == 1 {
				if req.Header.Get("X-Goog-Upload-Protocol") != "resumable" || req.Header.Get("X-Goog-Upload-Command") != "start" || req.Header.Get("X-Goog-Upload-Header-Content-Length") != "45000" {
					t.Fatal("invalid resumable initialization")
				}
				target := req.URL.String() + "?upload_id=synthetic"
				if untrusted {
					target = "https://evil.example/upload"
				}
				header.Set("X-Goog-Upload-URL", target)
			} else {
				body, err := io.ReadAll(req.Body)
				if err != nil || !bytes.Equal(body, raw) || req.ContentLength != int64(len(raw)) || req.Header.Get("X-Goog-Upload-Command") != "upload, finalize" {
					t.Fatal("upload bytes changed", err)
				}
				reply = `{"fileId":"synthetic-file"}`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply)), Header: header}, nil
		})}
		id, err := Upload(context.Background(), client, c, string(access), session, contract.Attachment{Filename: "input.txt", MIMEType: "text/plain", Data: raw})
		if untrusted {
			if err == nil || calls != 1 {
				t.Fatal("credentials followed an untrusted upload destination")
			}
		} else if err != nil || id != "synthetic-file" || calls != 2 {
			t.Fatal(id, err, calls)
		}
	}
}

func TestAdmissionChecksAuthorizationAndDeclaredLength(t *testing.T) {
	payload := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}]}]}`
	r := enabledRuntime()
	called := 0
	_, _, _, err := ReadBody(context.Background(), strings.NewReader(payload), -1, r, func() error {
		called++
		return Fail(401, "Authentication required")
	})
	if code(err) != 401 || called != 1 || r.Status()["active_requests"] != 0 {
		t.Fatal("authorization must precede admission", err, r.Status())
	}
	for _, length := range []int64{1, int64(len(payload)) - 1, int64(len(payload)) + 1} {
		_, _, _, err = ReadBody(context.Background(), strings.NewReader(payload), length, r)
		if code(err) != 400 || r.Status()["active_requests"] != 0 || r.Status()["inflight_bytes"] != int64(0) {
			t.Fatal("incorrect length escaped admission accounting", length, err, r.Status())
		}
	}
	_, _, lease, err := ReadBody(context.Background(), strings.NewReader(payload), 0, r)
	if err != nil || lease == nil || lease.Budget() < 200<<20 {
		t.Fatal("zero length with a body must reserve the unknown-length envelope", err)
	}
	lease.Release()
}

func TestCredentialReplacementRejectsLateAuthenticationAndModelProof(t *testing.T) {
	r := enabledRuntime()
	c := credentialFixture(t)
	started, finish := make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: syntheticTransport(func(req *http.Request) (*http.Response, error) {
		reply := syntheticPlusConfig
		if req.Method == http.MethodGet {
			close(started)
			<-finish
			reply = `window.WIZ_global_data={"Wybhlc":"synthetic-api-key"};`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply))}, nil
	})}
	epoch := r.CredentialEpoch("account:with:colon")
	done := make(chan error, 1)
	go func() {
		_, err := r.Token(context.Background(), c, client, "account:with:colon", "account:with:colon:"+c.Fingerprint()+":", true)
		done <- err
	}()
	<-started
	r.ForgetCredential("account:with:colon")
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.RecordModel("account:with:colon", map[string]any{"enterprise_web": c.MetadataValue()}, "observed-web-id", epoch)
	if len(r.tokens) != 0 || len(r.models) != 0 {
		t.Fatal("a pre-replacement result resurrected cached evidence")
	}
	r.RecordModel("account:with:colon", map[string]any{"enterprise_web": c.MetadataValue()}, "observed-web-id", r.CredentialEpoch("account:with:colon"))
	if len(r.models) != 1 {
		t.Fatal("current result was discarded")
	}
}

func TestPublicErrorDetailRejectsForgedMessages(t *testing.T) {
	if message, _ := PublicErrorDetail(&Failure{Code: 400, Message: "private upstream value"}, 400); message != "" {
		t.Fatal("untrusted message became public")
	}
	if message, _ := PublicErrorDetail(Fail(503, "Media input channel is disabled"), 503); message == "" {
		t.Fatal("safe local rejection was hidden")
	}
}

func TestMediaClassificationReusesOnlyTheUnchangedRequestBody(t *testing.T) {
	r := enabledRuntime()
	media := `{"contents":[{"role":"user","parts":[{"text":"read"},{"inlineData":{"mimeType":"image/png","data":"YQ=="}}]}]}`
	body, ctx, lease, err := ReadBody(context.Background(), strings.NewReader(media), int64(len(media)), r)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx = WithBody(ctx, body)
	if !HasMediaInContext(ctx, body) || !HasMediaInContext(ctx, bytes.Clone(body)) {
		t.Fatal("media was lost")
	}
	text := []byte(`{"contents":[{"role":"user","parts":[{"text":"\u4f60\u597d inlineData"}]}]}`)
	if HasMediaInContext(ctx, text) || HasMedia(text) {
		t.Fatal("a text replacement inherited media classification")
	}
	textCtx := WithBody(context.Background(), text)
	if HasMediaInContext(textCtx, text) || !HasMediaInContext(textCtx, body) {
		t.Fatal("replacement classification was not recomputed")
	}
}

func TestInlineNamedFieldsOutsideContentPartsStayOnTextPath(t *testing.T) {
	for _, payload := range []string{
		`{"generationConfig":{"responseSchema":{"properties":{"inlineData":{"type":"string"}}}},"contents":[{"parts":[{"text":"ordinary"}]}]}`,
		`{"contents":[{"parts":[{"functionCall":{"name":"example","args":{"inlineData":"ordinary"}}}]}]}`,
		`{"metadata":{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"YQ=="}}]}]},"contents":[{"parts":[{"text":"ordinary"}]}]}`,
	} {
		for _, fragment := range []int{1, 7, 4096} {
			var scanner KeyScanner
			for offset := 0; offset < len(payload); offset += fragment {
				if scanner.Feed([]byte(payload[offset:min(len(payload), offset+fragment)])) {
					t.Fatal("schema/argument metadata was mistaken for media")
				}
			}
			if HasMedia([]byte(payload)) {
				t.Fatal("metadata diverted an ordinary request")
			}
			_, _, lease, err := ReadBody(context.Background(), fragmentReader{strings.NewReader(payload), fragment}, int64(len(payload)), NewRuntime())
			if err != nil || lease != nil {
				t.Fatal("ordinary content consulted the disabled media channel", err)
			}
		}
	}
	if _, _, _, err := ReadBody(context.Background(), strings.NewReader(strings.Repeat("[", 10001)), -1, NewRuntime()); code(err) != 400 {
		t.Fatal("scanner nesting was not bounded", err)
	}
}
