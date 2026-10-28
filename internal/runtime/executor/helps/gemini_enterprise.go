package helps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	log "github.com/sirupsen/logrus"
)

// RecordEnterpriseFailure retains structural diagnostics in ordinary logs and
// request logs without retaining raw error bodies, policy matches or credentials.
func RecordEnterpriseFailure(ctx context.Context, cfg *config.Config, err error) {
	status := enterpriseFailureStatus(err)
	message, code, field := enterprise.PublicErrorDetail(err, status)
	var skipped *ge.SkippedError
	if errors.As(err, &skipped) {
		message, code = skipped.SkipMessage(), "upstream_skipped"
	}
	attempt := enterpriseAttemptFrom(ctx)
	if (message == "" || code == "invalid_field") && attempt == nil {
		return
	}
	if message == "" {
		message, code = "The upstream request could not be completed.", enterpriseFailureClass(err)
	}
	fields := log.Fields{"provider": ge.Provider, "stage": "upstream", "status": status, "code": code}
	if attempt != nil {
		for key, value := range attempt.fields(err) {
			fields[key] = value
		}
	}
	detail := map[string]any{"message": message, "code": code}
	if field != "" {
		fields["field"], detail["param"] = field, field
	}
	if upstream := enterprise.PublicUpstreamDiagnostic(err, status); upstream != nil {
		fields["upstream"], detail["upstream"] = upstream, upstream
	}
	if skipped != nil {
		fields["skip_reasons"], detail["skip_reasons"] = skipped.SkipReasons(), skipped.SkipReasons()
		if policy := skipped.PolicyDetails(); policy != nil {
			fields["policy"], detail["policy"] = policy, policy
		}
	}
	if raw, errMarshal := json.Marshal(map[string]any{"error": detail}); errMarshal == nil {
		RecordAPIResponseError(ctx, cfg, errors.New(string(raw)))
	}
	LogWithRequestID(ctx).WithFields(fields).Warn("Enterprise request failed")
}

// EnterpriseValidationFields records structural diagnostics without request values.
func EnterpriseValidationFields(stage string, err error) log.Fields {
	message, code, field := enterprise.PublicErrorDetail(err, http.StatusBadRequest)
	if message == "" {
		return nil
	}
	if stage != "input" && stage != "final_payload" {
		stage = "request"
	}
	return log.Fields{"provider": "gemini-enterprise", "stage": stage, "code": code, "field": field}
}

// EnterpriseResponseHeaders exposes names of ignored options without values or credentials.
func EnterpriseResponseHeaders(ignored []string) http.Header {
	headers := http.Header{}
	if len(ignored) > 0 {
		fields := make([]string, 0, min(len(ignored), 64))
		bytes := 0
		for _, field := range ignored {
			if strings.IndexFunc(field, func(r rune) bool { return r < ' ' || r > 126 }) >= 0 {
				field = strconv.QuoteToASCII(field)
			}
			if len(fields) >= 64 || bytes+len(field) > 4096 {
				break
			}
			fields = append(fields, field)
			bytes += len(field) + 1
		}
		if len(fields) > 0 {
			headers.Set("X-CLIProxyAPI-Ignored-Parameters", strings.Join(fields, ","))
		}
	}
	return headers
}
