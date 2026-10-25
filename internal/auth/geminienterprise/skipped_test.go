package geminienterprise

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSkippedErrorSanitizesReasonsAndStopsReplay(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"non-assist", `["NON_ASSIST_SEEKING_QUERY_IGNORED"]`, []string{"NON_ASSIST_SEEKING_QUERY_IGNORED"}},
		{"policy", `["CUSTOMER_POLICY_VIOLATION"]`, []string{"CUSTOMER_POLICY_VIOLATION"}},
		{"combined", `["CUSTOMER_POLICY_VIOLATION","NON_ASSIST_SEEKING_QUERY_IGNORED","CUSTOMER_POLICY_VIOLATION"]`, []string{"CUSTOMER_POLICY_VIOLATION", "NON_ASSIST_SEEKING_QUERY_IGNORED"}},
		{"empty", `[]`, []string{unspecifiedSkipReason}},
		{"missing", ``, []string{unspecifiedSkipReason}},
		{"malformed", `{`, []string{unspecifiedSkipReason}},
		{"unrecognized", `["synthetic-secret",{"private":"synthetic-secret"},7]`, []string{unspecifiedSkipReason}},
		{"oversized", `["` + strings.Repeat("synthetic-secret", 400) + `"]`, []string{unspecifiedSkipReason}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, started := range []bool{false, true} {
				err := NewSkippedError(json.RawMessage(tc.raw), started)
				if !err.IsRequestScoped() || !err.IsRequestStop() || err.GenerationStarted() != started || err.StatusCode() != 400 {
					t.Fatal("skip rejection lost request scope or replay protection")
				}
				if !reflect.DeepEqual(err.SkipReasons(), tc.want) {
					t.Fatalf("reasons=%v want=%v", err.SkipReasons(), tc.want)
				}
				var body struct {
					Error struct {
						Code, Message, Type string
						Reasons             []string `json:"skip_reasons"`
						Retryable           *bool    `json:"retryable"`
					}
				}
				if json.Unmarshal([]byte(err.Error()), &body) != nil || body.Error.Code != "upstream_skipped" || body.Error.Type != "invalid_request_error" || !reflect.DeepEqual(body.Error.Reasons, tc.want) || body.Error.Retryable == nil || *body.Error.Retryable {
					t.Fatalf("invalid public skip error: %s", err.Error())
				}
				if strings.Contains(err.Error(), "synthetic-secret") {
					t.Fatal("unrecognized upstream data leaked")
				}
				copyReasons := err.SkipReasons()
				copyReasons[0] = "modified"
				if !reflect.DeepEqual(err.SkipReasons(), tc.want) {
					t.Fatal("reason accessor exposed mutable state")
				}
			}
		})
	}
}
