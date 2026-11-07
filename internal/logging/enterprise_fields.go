package logging

import (
	"strconv"

	log "github.com/sirupsen/logrus"
)

// This projection accepts primitive observations only. In particular, adding an
// arbitrary error, upstream object or metadata map cannot print its contents.
var enterpriseDiagnosticFieldOrder = []string{
	"diagnostic_request_id", "caller_request_id", "trace_id", "upstream_request_id",
	"stage", "diagnostic_source", "status", "code", "error_class",
	"upstream_http_status", "upstream_rpc_status", "upstream_code", "google_status", "google_reason",
	"reason_class", "reason_evidence", "field", "upstream_fields",
	"upstream_model", "upstream_content_type", "stream", "request_sent", "generation_started", "output_seen",
	"location", "binding_ref", "binding_revision", "request_body_bytes", "query_chars", "query_bytes",
	"configured_fields", "unknown_field_count", "search_enabled",
	"build_ms", "connect_ms", "tls_ms", "headers_ms", "first_byte_ms", "first_output_ms", "body_ms", "response_ms", "total_ms",
	"skip_reasons", "policy_verdict", "policy_source", "policy_checks",
}

func enterpriseDiagnosticLogFields(data log.Fields) []string {
	return diagnosticLogFields(data, enterpriseDiagnosticFieldOrder)
}

var enterpriseWebDiagnosticFieldOrder = []string{
	"stage", "status", "field", "limit_name", "observed", "maximum", "observed_at_least",
	"contents", "logical_contents", "attachments", "attachment_bytes", "filtered_thoughts", "filtered_signatures",
}

func enterpriseWebDiagnosticLogFields(data log.Fields) []string {
	return diagnosticLogFields(data, enterpriseWebDiagnosticFieldOrder)
}

func diagnosticLogFields(data log.Fields, order []string) []string {
	var fields []string
	for _, key := range order {
		value, ok := data[key]
		if !ok {
			continue
		}
		var text string
		switch v := value.(type) {
		case string:
			if len(v) > 4096 {
				text = `"truncated"`
			} else {
				text = strconv.Quote(v)
			}
		case int:
			if (key == "upstream_http_status" || key == "upstream_code") && v == 0 {
				text = "unknown"
			} else {
				text = strconv.Itoa(v)
			}
		case int64:
			text = strconv.FormatInt(v, 10)
		case uint64:
			text = strconv.FormatUint(v, 10)
		case bool:
			text = strconv.FormatBool(v)
		default:
			continue
		}
		fields = append(fields, key+"="+text)
	}
	return fields
}
