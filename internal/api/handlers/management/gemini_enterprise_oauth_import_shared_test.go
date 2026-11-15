package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type sharedImportTestObject struct {
	body []byte
	etag string
}

type sharedImportTestBucket struct {
	objects        map[string]sharedImportTestObject
	objectWrites   int
	credentialPUTs int
	finishFails    bool
	dropSaveReply  bool
}

func (b *sharedImportTestBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/fixture-bucket/")
	obj, present := b.objects[key]
	writeError := func(status int, code string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", code)
	}
	if r.Method == http.MethodGet {
		if r.URL.Query().Get("list-type") == "2" {
			var listing struct {
				XMLName     xml.Name `xml:"ListBucketResult"`
				IsTruncated bool
				Contents    []struct{ Key string }
			}
			for name := range b.objects {
				if strings.HasPrefix(name, r.URL.Query().Get("prefix")) {
					listing.Contents = append(listing.Contents, struct{ Key string }{name})
				}
			}
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(listing)
			return
		}
		if !present {
			writeError(http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", obj.etag)
		_, _ = w.Write(obj.body)
		return
	}
	if r.Method != http.MethodPut {
		writeError(http.StatusMethodNotAllowed, "MethodNotAllowed")
		return
	}
	b.objectWrites++
	if r.Header.Get("If-None-Match") == "*" && present ||
		r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != obj.etag {
		writeError(http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	if b.finishFails && strings.Contains(key, "/requests/") && r.Header.Get("If-Match") != "" {
		writeError(http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(http.StatusBadRequest, "IncompleteBody")
		return
	}
	credential := strings.HasPrefix(key, "isolated/auths/")
	if credential {
		b.credentialPUTs++
	}
	etag := fmt.Sprintf("\"%x\"", sha256.Sum256(body))
	b.objects[key] = sharedImportTestObject{body, etag}
	if credential && b.dropSaveReply {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func useSharedImportTestStore(t *testing.T, h *Handler, endpoint string, enabled bool) {
	t.Helper()
	tokenStore, err := store.NewObjectTokenStore(store.ObjectStoreConfig{
		Endpoint: endpoint, Bucket: "fixture-bucket", Prefix: "isolated", Region: "us-east-1",
		AccessKey: "fixture", SecretKey: "fixture-only", PathStyle: true, LocalRoot: t.TempDir(),
		EnterpriseOAuthImportCAS: enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.AuthDir = tokenStore.AuthDir()
	h.tokenStore, h.authManager = tokenStore, coreauth.NewManager(tokenStore, nil, nil)
}

func TestEnterpriseOAuthObjectImportLostRepliesAndRuntimeAreInstanceLocal(t *testing.T) {
	for _, lostSave := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-save=", lostSave), func(t *testing.T) {
			x := newEnterpriseImportTest(t)
			bucket := &sharedImportTestBucket{objects: make(map[string]sharedImportTestObject), finishFails: true, dropSaveReply: lostSave}
			server := httptest.NewServer(bucket)
			t.Cleanup(server.Close)
			useSharedImportTestStore(t, x.h, server.URL, true)
			r := x.request("fixture-shared-runtime")
			result := x.post(t, r)
			if result["status"] != "saved" || result["runtime_synced"] != !lostSave ||
				result["authorization_revision"] != float64(1) || bucket.credentialPUTs != 1 || x.calls != 1 {
				t.Fatalf("unexpected import result: %v; refreshes=%d credential PUTs=%d", result, x.calls, bucket.credentialPUTs)
			}
			if !lostSave {
				credentialPath := filepath.Join(x.h.cfg.AuthDir, result["name"].(string))
				if _, err := os.Stat(credentialPath); err != nil {
					t.Fatal(err)
				}
				// Raw upload and both delete modes must reject before changing the mirror.
				router := gin.New()
				router.POST("/credentials", x.h.UploadAuthFile)
				router.DELETE("/credentials", x.h.DeleteAuthFile)
				router.PATCH("/credentials/fields", x.h.PatchAuthFileFields)
				for _, request := range []*http.Request{
					httptest.NewRequest(http.MethodPost, "/credentials?name="+result["name"].(string), strings.NewReader(`{"type":"vertex","label":"fixture"}`)),
					httptest.NewRequest(http.MethodDelete, "/credentials?name="+result["name"].(string), nil),
					httptest.NewRequest(http.MethodDelete, "/credentials?all=true", nil),
					httptest.NewRequest(http.MethodPatch, "/credentials/fields", strings.NewReader(fmt.Sprintf(
						`{"name":%q,"refresh_token":"synthetic-private-overwrite"}`, result["name"]))),
				} {
					w := httptest.NewRecorder()
					router.ServeHTTP(w, request)
					if w.Code != http.StatusConflict {
						t.Fatalf("generic %s got %d", request.Method, w.Code)
					}
					if _, err := os.Stat(credentialPath); err != nil {
						t.Fatal("generic CRUD removed the Enterprise mirror")
					}
				}
			}

			writes := bucket.objectWrites
			useSharedImportTestStore(t, x.h, server.URL, true)
			if got := x.get(t, r.RequestID); got["status"] != "saved" || got["runtime_synced"] != false || bucket.objectWrites != writes {
				t.Fatalf("readback result from another process: %v", got)
			}
			if len(x.h.authManager.List()) != 0 {
				t.Fatal("read-only lookup published a credential")
			}
			credentialPath := filepath.Join(x.h.cfg.AuthDir, result["name"].(string))
			if _, err := os.Stat(credentialPath); !os.IsNotExist(err) {
				t.Fatalf("read-only lookup changed an empty mirror: %v", err)
			}
			replay := x.post(t, r)
			if replay["status"] != "saved" || x.calls != 1 || bucket.objectWrites != writes {
				t.Fatalf("same request replayed: %v", replay)
			}
			r.RefreshToken = "synthetic-private-different"
			if conflict := x.post(t, r); conflict["code"] != "request_id_conflict" || x.calls != 1 {
				t.Fatalf("same request accepted a different digest: %v", conflict)
			}
			publications := 0
			x.h.authManager.SetEnterpriseAccountSyncHook(func(ctx context.Context, saved *coreauth.Auth) error {
				publications++
				if !coreauth.IsEnterpriseOAuthRuntimeSync(ctx) || coreauth.EnterpriseOAuthAuthorizationRevision(saved) != 1 {
					t.Error("publication was not fenced to the saved revision")
				}
				return nil
			})
			for range 2 {
				if got := x.runtimeSync(t, r.RequestID, ""); got["status"] != "saved" || got["runtime_synced"] != true {
					t.Fatalf("runtime publication: %v", got)
				}
			}
			if publications != 1 || x.calls != 1 || bucket.objectWrites != writes || bucket.credentialPUTs != 1 {
				t.Fatalf("readback or publication repeated a write: publications=%d refreshes=%d writes=%d", publications, x.calls, bucket.objectWrites)
			}
			// Manager binds to this runtime, then uses the existing credential
			// download endpoint. Recovery must leave a usable local mirror.
			router := gin.New()
			router.GET("/credentials/download", x.h.DownloadAuthFile)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/credentials/download?name="+result["name"].(string), nil))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "\"enterprise_oauth_import_receipts\"") {
				t.Fatalf("recovered credential download failed with HTTP %d", w.Code)
			}
			if got := x.get(t, r.RequestID); got["runtime_synced"] != true {
				t.Fatalf("published exact revision was not confirmed: %v", got)
			}
		})
	}
}

func TestEnterpriseOAuthObjectStoreCapabilityIsOptIn(t *testing.T) {
	x := newEnterpriseImportTest(t)
	bucket := &sharedImportTestBucket{objects: make(map[string]sharedImportTestObject)}
	server := httptest.NewServer(bucket)
	t.Cleanup(server.Close)
	for _, enabled := range []bool{false, true} {
		useSharedImportTestStore(t, x.h, server.URL, enabled)
		r := enterpriseManagementRequest(t, x.h.GeminiEnterpriseOAuthCapabilities, http.MethodGet, nil)
		metadata := importResponse(t, r)
		if metadata["protocol_version"] != float64(1) || metadata["client_secret_ready"] != true || metadata["import_supported"] != enabled || bucket.objectWrites != 0 {
			t.Fatalf("capability with configured opt-in=%t: %v", enabled, metadata)
		}
	}
}

func TestEnterpriseOAuthObjectStoreKeepsVertexImport(t *testing.T) {
	x := newEnterpriseImportTest(t)
	bucket := &sharedImportTestBucket{objects: make(map[string]sharedImportTestObject)}
	server := httptest.NewServer(bucket)
	t.Cleanup(server.Close)
	useSharedImportTestStore(t, x.h, server.URL, true)

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(file, `{"type":"authorized_user","client_id":"fixture-client","client_secret":"synthetic-private-vertex","refresh_token":"synthetic-private-vertex-refresh","quota_project_id":"fixture-project"}`); err != nil {
		t.Fatal(err)
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v8/management/oauth/import?provider=vertex", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	router := gin.New()
	router.POST("/v8/management/oauth/import", x.h.ImportOAuthV8)
	router.ServeHTTP(w, req)
	got := importResponse(t, w)
	if w.Code != http.StatusOK || got["status"] != "ok" || x.calls != 0 || bucket.objectWrites != 1 ||
		bucket.objects["isolated/auths/vertex-adc-fixture-project.json"].etag == "" {
		t.Fatalf("Vertex import status=%d result=%v writes=%d", w.Code, got, bucket.objectWrites)
	}
}
