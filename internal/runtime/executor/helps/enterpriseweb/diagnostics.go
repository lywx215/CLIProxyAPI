package enterpriseweb

import (
	"context"
	"errors"
	"regexp"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	log "github.com/sirupsen/logrus"
)

// Stage diagnostics deliberately exclude payloads, account data and raw errors.
func ObserveStage(ctx context.Context, stage, model string, summary contract.ContextSummary) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	log.WithFields(log.Fields{
		"request_id": logging.GetRequestID(ctx), "provider": Provider, "stage": stage,
		"model": model, "contents": summary.Contents, "logical_contents": summary.LogicalContents,
		"attachments": summary.Attachments, "attachment_bytes": summary.AttachmentBytes,
		"filtered_thoughts": summary.FilteredThoughts, "filtered_signatures": summary.FilteredSignatures,
	}).Debug("Web request stage")
}

var diagnosticInputField = regexp.MustCompile(`^(?:body|generationConfig|tools|toolConfig|inlineData|contents(?:\[[0-9]{1,5}\])?(?:\.parts(?:\[[0-9]{1,5}\])?)?(?:\.(?:role|text|thought|thoughtSignature|inlineData(?:\.(?:mimeType|data))?|fileData|functionCall|functionResponse|executableCode|codeExecutionResult)(?:\.(?:name|id|args|response|language|code|outcome|output))?)?|systemInstruction(?:\.parts(?:\[[0-9]{1,5}\])?)?(?:\.(?:role|text))?)$`)

func observePreflight(ctx context.Context, reason string, err error) {
	fields := log.Fields{"request_id": logging.GetRequestID(ctx), "provider": Provider, "stage": "preflight", "reason": preflightReason(reason)}
	var failure *Failure
	if errors.As(err, &failure) {
		fields["status"] = failure.Code
		if diagnosticInputField.MatchString(failure.Param) {
			fields["field"] = failure.Param
		}
		if failure.Code == 400 && failure.AttachmentReason.Valid() {
			fields["attachment_reason"] = string(failure.AttachmentReason)
		}
		if failure.Limit != nil && failure.Limit.Valid() {
			fields["limit_name"] = string(failure.Limit.Name)
			fields["observed"] = failure.Limit.Observed
			fields["maximum"] = failure.Limit.Maximum
			fields["observed_at_least"] = failure.Limit.AtLeast
		}
	}
	entry := log.WithFields(fields)
	if failure != nil && failure.Code == 413 {
		entry.Warn("Web request rejected before session creation")
	} else {
		entry.Debug("Web request rejected before session creation")
	}
}
