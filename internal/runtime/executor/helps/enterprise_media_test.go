package helps

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
)

type mediaTransport func(*http.Request) (*http.Response, error)

func (f mediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnterpriseMediaDownloadSafety(t *testing.T) {
	var pngData bytes.Buffer
	_ = png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	engine := "projects/p/locations/global/collections/default_collection/engines/e"
	refs := EnterpriseMediaRefs{Session: engine + "/sessions/123", FileID: "file-id", MIME: "image/png"}
	validLocation := "https://discoveryengine.googleapis.com/download/v1alpha/" + refs.Session + ":downloadFile?fileId=file-id&alt=media"
	for _, tc := range []struct {
		name, location, encoded, mime string
		bad                           bool
	}{
		{"valid", validLocation, base64.StdEncoding.EncodeToString(pngData.Bytes()), "image/png", false},
		{"foreign", "https://evil.example/download?fileId=file-id", "", "image/png", true},
		{"http", strings.Replace(validLocation, "https:", "http:", 1), "", "image/png", true},
		{"session", strings.Replace(validLocation, "sessions/123", "sessions/456", 1), "", "image/png", true},
		{"file", strings.Replace(validLocation, "file-id", "other-id", 1), "", "image/png", true},
		{"duplicate", validLocation + "&fileId=file-id", "", "image/png", true},
		{"base64", validLocation, "invalid!!!", "image/png", true},
		{"mime", validLocation, base64.StdEncoding.EncodeToString(pngData.Bytes()), "image/jpeg", true},
		{"unknown-mime", validLocation, "", "application/octet-stream", true},
		{"oversize", validLocation, base64.StdEncoding.EncodeToString(make([]byte, EnterpriseMediaMaxBytes+1)), "image/png", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "discoveryengine.googleapis.com" || r.Header.Get("Authorization") != "Bearer test" {
					t.Fatal("credential isolation failed")
				}
				if _, ok := r.Context().Deadline(); ok {
					t.Fatal("download has a deadline")
				}
				if calls == 1 {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": {tc.location}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.encoded))}, nil
			})}
			prepare := func(r *http.Request) error { r.Header.Set("Authorization", "Bearer test"); return nil }
			current := refs
			current.MIME = tc.mime
			data, err := DownloadEnterpriseMedia(context.Background(), client, prepare, engine, "image", current)
			if tc.bad {
				if err == nil {
					t.Fatal("accepted unsafe download")
				}
				if !err.(interface{ IsRequestScoped() bool }).IsRequestScoped() {
					t.Fatal("download failure could retry generation")
				}
			} else if err != nil || !bytes.Equal(data, pngData.Bytes()) || calls != 2 {
				t.Fatalf("download failed: %v", err)
			}
		})
	}
}

func TestEnterpriseMediaReferenceDeduplication(t *testing.T) {
	refs := EnterpriseMediaRefs{}
	file := enterprise.Event{Type: "file", FileID: "one", MIME: "video/mp4"}
	for i := 0; i < 3; i++ {
		if err := refs.Add(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := refs.Add(enterprise.Event{Type: "file", FileID: "two", MIME: "video/mp4"}); err == nil {
		t.Fatal("accepted multiple files")
	}
}

func TestEnterpriseRegionalMediaDownloads(t *testing.T) {
	var pngData bytes.Buffer
	_ = png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	for _, region := range ge.SupportedLocations() {
		for _, redirectHost := range []string{"same", "cross-region", "foreign", "global-alias"} {
			t.Run(region+"/"+redirectHost, func(t *testing.T) {
				engine := "projects/p/locations/" + region + "/collections/default_collection/engines/e"
				refs := EnterpriseMediaRefs{Session: engine + "/sessions/123", FileID: "file-id", MIME: "image/png"}
				origin := ge.OriginForLocation(region)
				redirectOrigin := origin
				switch redirectHost {
				case "cross-region":
					if region == "eu" {
						redirectOrigin = ge.OriginForLocation("us")
					} else {
						redirectOrigin = ge.OriginForLocation("eu")
					}
				case "foreign":
					redirectOrigin = "https://us-discoveryengine.googleapis.com.evil.test"
				case "global-alias":
					redirectOrigin = "https://global-discoveryengine.googleapis.com"
				}
				calls := 0
				client := &http.Client{Transport: mediaTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if !ge.OfficialBinding(r, "p", region) || r.Header.Get("Authorization") != "Bearer synthetic" {
						t.Fatal("credentials sent to wrong media region", r.URL)
					}
					if calls == 1 {
						return &http.Response{StatusCode: 302, Header: http.Header{"Location": {redirectOrigin + "/download/v1alpha/" + refs.Session + ":downloadFile?fileId=file-id&alt=media"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(base64.StdEncoding.EncodeToString(pngData.Bytes())))}, nil
				})}
				data, err := DownloadEnterpriseMedia(context.Background(), client, func(r *http.Request) error { r.Header.Set("Authorization", "Bearer synthetic"); return nil }, engine, "image", refs)
				valid := redirectHost == "same" || (region == "global" && redirectHost == "global-alias")
				if valid {
					if err != nil || !bytes.Equal(data, pngData.Bytes()) || calls != 2 {
						t.Fatal("regional download failed", err, calls)
					}
				} else if err == nil || calls != 1 {
					t.Fatal("unsafe redirect followed", calls)
				}
			})
		}
	}
}
