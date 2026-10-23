package helps

import (
	"net/http"
	"strconv"
	"strings"

	enterprise "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/geminienterprise"
	log "github.com/sirupsen/logrus"
)

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
