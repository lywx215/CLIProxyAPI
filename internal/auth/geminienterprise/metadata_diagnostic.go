package geminienterprise

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// MetadataDiagnostic contains only validated, public Google error attributes.
type MetadataDiagnostic struct {
	HTTPStatus      int    `json:"http_status,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Service         string `json:"service,omitempty"`
	ConsumerProject string `json:"consumer_project,omitempty"`
}

type MetadataError struct {
	MetadataDiagnostic
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *MetadataError) Error() string { return "gemini-enterprise: " + e.Message }

// ParseMetadataError never copies Google's free-form message or arbitrary metadata.
func ParseMetadataError(status int, body []byte) *MetadataError {
	e := &MetadataError{MetadataDiagnostic: MetadataDiagnostic{HTTPStatus: status}, Code: fmt.Sprintf("http_%d", status), Message: "Google metadata request failed."}
	switch status {
	case http.StatusUnauthorized:
		e.Code, e.Message = "unauthorized", "Google authorization could not be verified."
	case http.StatusForbidden:
		e.Code, e.Message = "permission_denied", "Google denied access to this metadata."
	case http.StatusNotFound:
		e.Code, e.Message = "not_found", "The requested metadata was not found."
	case http.StatusTooManyRequests:
		e.Code, e.Message = "rate_limited", "Google metadata requests are rate limited."
	}
	var envelope struct {
		Error struct {
			Details []struct {
				Type     string            `json:"@type"`
				Domain   string            `json:"domain"`
				Reason   string            `json:"reason"`
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if len(body) > 64*1024 || json.Unmarshal(body, &envelope) != nil {
		return e
	}
	for _, detail := range envelope.Error.Details {
		if detail.Type != "type.googleapis.com/google.rpc.ErrorInfo" || (detail.Domain != "googleapis.com" && detail.Domain != "discoveryengine.googleapis.com" && detail.Domain != "serviceusage.googleapis.com") {
			continue
		}
		code, message := "", ""
		switch detail.Reason {
		case "SERVICE_DISABLED":
			code, message = "service_disabled", "The required Google API is not enabled or has been disabled."
		case "BILLING_DISABLED":
			code, message = "billing_disabled", "Google reports that billing is disabled for the consumer project."
		case "CONSUMER_SUSPENDED":
			code, message = "consumer_suspended", "Google reports that the consumer project is suspended."
		case "IAM_PERMISSION_DENIED", "ACCESS_DENIED":
			code, message = "permission_denied", "Google denied access to this metadata."
		}
		if code == "" || status != http.StatusForbidden {
			continue
		}
		if e.Reason != "" && e.Reason != detail.Reason {
			return &MetadataError{MetadataDiagnostic: MetadataDiagnostic{HTTPStatus: status}, Code: "permission_denied", Message: "Google denied access to this metadata."}
		}
		e.Code, e.Message, e.Reason = code, message, detail.Reason
		switch detail.Metadata["service"] {
		case "discoveryengine.googleapis.com", "cloudresourcemanager.googleapis.com", "serviceusage.googleapis.com", "cloudbilling.googleapis.com":
			e.Service = detail.Metadata["service"]
		}
		consumer := strings.TrimPrefix(detail.Metadata["consumer"], "projects/")
		if numericProject(consumer) {
			e.ConsumerProject = consumer
		}
	}
	return e
}

func CatalogError(err error) *MetadataError {
	var metadata *MetadataError
	if errors.As(err, &metadata) {
		copyError := *metadata
		return &copyError
	}
	return &MetadataError{Code: "catalog_unavailable", Message: "The upstream model catalog could not be read."}
}
