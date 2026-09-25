package diagnostics

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type vector struct {
	ID       string          `json:"id"`
	Input    json.RawMessage `json:"input"`
	Expected json.RawMessage `json:"expected"`
}

func vectors(t *testing.T, name string) []vector {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "contracts", "diagnostics", "v1", "vectors", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []vector
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}
func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}
func equalJSON(t *testing.T, got any, expected []byte) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, z any
	decode(t, b, &a)
	decode(t, expected, &z)
	if !reflect.DeepEqual(a, z) {
		t.Fatalf("got %s\nwant %s", b, expected)
	}
}
func fields(pairs [][2]string) http.Header {
	h := make(http.Header)
	for _, pair := range pairs {
		h.Add(pair[0], pair[1])
	}
	return h
}
func lowerHeaders(h http.Header) http.Header {
	out := make(http.Header)
	for k, values := range h {
		out[strings.ToLower(k)] = append(out[strings.ToLower(k)], values...)
	}
	return out
}
func compareIncoming(t *testing.T, got Incoming, want json.RawMessage) {
	t.Helper()
	var expected Incoming
	decode(t, want, &expected)
	if expected.TraceID == "<generated>" {
		if !nonzeroHex(got.TraceID, 32) {
			t.Fatalf("invalid generated trace ID")
		}
		got.TraceID = "<generated>"
	}
	equalJSON(t, got, want)
}

func TestSharedHeadersAndHTTPIngress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, group := range []string{"headers", "http-ingress"} {
		for _, tc := range vectors(t, group) {
			t.Run(group+"/"+tc.ID, func(t *testing.T) {
				var in struct {
					Headers                                 [][2]string
					Authenticated, ConfiguredInboundAdapter bool
					WireHeaderLines                         []string
				}
				decode(t, tc.Input, &in)
				if group == "headers" {
					compareIncoming(t, Extract(fields(in.Headers), in.Authenticated, in.ConfiguredInboundAdapter), tc.Expected)
					return
				}
				wire := "GET /fixture HTTP/1.1\r\nHost: fixture.test\r\n" + strings.Join(in.WireHeaderLines, "\r\n") + "\r\n\r\n"
				req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(wire)))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(lowerHeaders(req.Header), lowerHeaders(fields(in.Headers))) {
					t.Fatal("framework fields differ from shared fixture")
				}
				router := gin.New()
				router.GET("/fixture", func(c *gin.Context) {
					compareIncoming(t, Extract(c.Request.Header, false, false), tc.Expected)
					c.String(201, "unchanged")
				})
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != 201 || w.Body.String() != "unchanged" {
					t.Fatal("HTTP behavior changed")
				}
			})
		}
	}
}

func TestSharedPeers(t *testing.T) {
	for _, tc := range vectors(t, "peers") {
		t.Run(tc.ID, func(t *testing.T) {
			var in struct {
				Config json.RawMessage
				Target string
			}
			decode(t, tc.Input, &in)
			raw := string(in.Config)
			if len(raw) > 0 && raw[0] == '"' {
				decode(t, in.Config, &raw)
			}
			if raw == "null" {
				raw = ""
			}
			p := ParsePeers(raw)
			var match *string
			if u, err := url.Parse(in.Target); err == nil {
				if peer := p.Match(u); peer != nil {
					match = &peer.Alias
				}
			}
			equalJSON(t, map[string]any{"configStatus": p.Status, "match": match}, tc.Expected)
		})
	}
}

type outboundInput struct {
	RequestID, TraceID, CallSpanID, Flags string
	Tracestate                            *string
	Headers                               [][2]string
	Allowed, DiagnosticOwned, Response    bool
}

func TestSharedOutbound(t *testing.T) {
	for _, tc := range vectors(t, "outbound") {
		t.Run(tc.ID, func(t *testing.T) {
			var in outboundInput
			decode(t, tc.Input, &in)
			req := httptest.NewRequest("POST", "https://example.test/v1", nil)
			req.Header = fields(in.Headers)
			if in.DiagnosticOwned {
				*req = *req.WithContext(context.WithValue(req.Context(), ownershipKey{}, ownership{req}))
			}
			if in.Response {
				ResponseHeaders(req.Header, in.RequestID, in.TraceID)
			} else {
				req = prepareRequest(req, Incoming{TraceID: in.TraceID, Flags: in.Flags, Tracestate: in.Tracestate}, in.RequestID, in.CallSpanID, in.Allowed)
			}
			equalJSON(t, map[string]any{"headers": lowerHeaders(req.Header), "diagnosticOwned": requestOwned(req) && !in.Response}, tc.Expected)
		})
	}
}

func TestSharedRedirectPolicy(t *testing.T) {
	for _, tc := range vectors(t, "redirects") {
		t.Run(tc.ID, func(t *testing.T) {
			var in struct {
				Peers   json.RawMessage
				Headers [][2]string
				Steps   []struct {
					outboundInput
					Target          string
					BusinessHeaders [][2]string
				}
			}
			decode(t, tc.Input, &in)
			peers := ParsePeers(string(in.Peers))
			req := httptest.NewRequest("GET", "https://example.test", nil)
			req.Header = fields(in.Headers)
			var got []any
			for _, step := range in.Steps {
				// These vectors model clients carrying the injected headers. Clean
				// that exact owned request before copying and business writes. Go's
				// actual pristine-ireq behavior is tested separately over local HTTP.
				cleanHeaders(req.Header, requestOwned(req))
				req = req.WithContext(context.WithValue(req.Context(), ownershipKey{}, ownership{}))
				for _, pair := range step.BusinessHeaders {
					req.Header.Add(pair[0], pair[1])
				}
				req.URL, _ = url.Parse(step.Target)
				allowed := peers.Match(req.URL) != nil
				if allowed != step.Allowed {
					t.Fatal("final URL policy")
				}
				req = prepareRequest(req, Incoming{TraceID: step.TraceID, Flags: step.Flags, Tracestate: step.Tracestate}, step.RequestID, step.CallSpanID, allowed)
				got = append(got, map[string]any{"headers": lowerHeaders(req.Header.Clone()), "diagnosticOwned": requestOwned(req)})
			}
			equalJSON(t, got, tc.Expected)
		})
	}
}

func TestSharedPeerResponsesAndSources(t *testing.T) {
	for _, tc := range vectors(t, "peer-response") {
		t.Run(tc.ID, func(t *testing.T) {
			var in struct {
				Headers        [][2]string
				PeerConfigured bool
			}
			decode(t, tc.Input, &in)
			equalJSON(t, ReadPeerIDs(fields(in.Headers), in.PeerConfigured), tc.Expected)
		})
	}
	for _, tc := range vectors(t, "copy-source") {
		t.Run(tc.ID, func(t *testing.T) {
			var in struct{ Headers [][2]string }
			decode(t, tc.Input, &in)
			out := make([][2]string, 0)
			for _, p := range in.Headers {
				if !IsPropagationHeader(p[0]) {
					out = append(out, p)
				}
			}
			equalJSON(t, out, tc.Expected)
			if !reflect.DeepEqual(lowerHeaders(CopyInboundHeaders(fields(in.Headers))), lowerHeaders(fields(out))) {
				t.Fatal("automatic source copy differs from vector")
			}
		})
	}
	for _, tc := range vectors(t, "source-scope") {
		t.Run(tc.ID, func(t *testing.T) {
			var in struct{ Left, Right SourceScope }
			decode(t, tc.Input, &in)
			equalJSON(t, CompareSourceScope(in.Left, in.Right), tc.Expected)
		})
	}
}

func TestSharedResources(t *testing.T) {
	for _, tc := range vectors(t, "resources") {
		t.Run(tc.ID, func(t *testing.T) {
			var in struct {
				Config                   ResourceConfig
				PlatformID, LocalService string
			}
			decode(t, tc.Input, &in)
			r, _ := NewResource(in.Config, in.PlatformID)
			other, _ := NewResource(in.Config, in.PlatformID)
			if _, err := uuid.Parse(r.BootID); err != nil || r.BootID == other.BootID {
				t.Fatal("boot lifecycle")
			}
			if r.Service != "cliproxyapi" {
				t.Fatal("binary service must be fixed")
			}
			var want map[string]any
			decode(t, tc.Expected, &want)
			if want["instanceId"] == "<random-uuid>" {
				if _, err := uuid.Parse(r.InstanceID); err != nil || r.InstanceID == other.InstanceID {
					t.Fatal("instance fallback")
				}
				r.InstanceID = "<random-uuid>"
			}
			// Foreign-service vectors exercise the same worker/restart algorithm;
			// their service literal is not configurable in this binary.
			equalJSON(t, map[string]any{"environment": r.Environment, "deploymentId": r.DeploymentID, "nodeLabel": r.NodeLabel, "instanceId": r.InstanceID, "instanceIdentitySource": r.InstanceIdentitySource, "service": in.LocalService, "bootId": "<new-uuid-per-worker>"}, tc.Expected)
		})
	}
}

func TestSharedCoverage(t *testing.T) {
	for _, tc := range vectors(t, "coverage") {
		t.Run(tc.ID, func(t *testing.T) {
			var in CoverageEvidence
			decode(t, tc.Input, &in)
			equalJSON(t, AssessCoverage(in), tc.Expected)
		})
	}
}
