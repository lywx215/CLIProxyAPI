package contract

type InputLimitName string

const (
	LimitRequestBytes          InputLimitName = "request_bytes"
	LimitDeclaredRequestBytes  InputLimitName = "declared_request_bytes"
	LimitTextBytes             InputLimitName = "text_bytes"
	LimitGenerationConfigBytes InputLimitName = "generation_config_bytes"
	LimitJSONObjectFields      InputLimitName = "json_object_fields"
	LimitHistorySegments       InputLimitName = "history_segments"
	LimitHistoryContents       InputLimitName = "history_contents"
	LimitHistoryTextBytes      InputLimitName = "history_text_bytes"
	LimitHistoryOptionsBytes   InputLimitName = "history_options_bytes"
	LimitHistoryOptionsDepth   InputLimitName = "history_options_depth"
	LimitHistoryOptionsNodes   InputLimitName = "history_options_nodes"
	LimitUserParts             InputLimitName = "user_parts"
	LimitModelParts            InputLimitName = "model_parts"
	LimitSystemParts           InputLimitName = "system_parts"
	LimitAttachments           InputLimitName = "attachments"
	LimitAttachmentBytes       InputLimitName = "attachment_bytes"
	LimitToolDepth             InputLimitName = "tool_depth"
	LimitToolNodes             InputLimitName = "tool_nodes"
	LimitToolArrayEntries      InputLimitName = "tool_array_entries"
)

// InputLimit retains measurements only. AtLeast marks an early bounded scan,
// rather than claiming that the complete input was counted or decoded.
type InputLimit struct {
	Name              InputLimitName
	Observed, Maximum int64
	AtLeast           bool
}

func (l InputLimit) Valid() bool {
	switch l.Name {
	case LimitRequestBytes, LimitDeclaredRequestBytes, LimitTextBytes, LimitGenerationConfigBytes, LimitJSONObjectFields,
		LimitHistorySegments, LimitHistoryContents, LimitHistoryTextBytes, LimitHistoryOptionsBytes,
		LimitHistoryOptionsDepth, LimitHistoryOptionsNodes, LimitUserParts,
		LimitModelParts, LimitSystemParts, LimitAttachments, LimitAttachmentBytes,
		LimitToolDepth, LimitToolNodes, LimitToolArrayEntries:
		return l.Maximum >= 0 && l.Observed > l.Maximum
	default:
		return false
	}
}

func LimitFailure(field, message string, limit InputLimit) *Failure {
	return &Failure{Code: 413, Status: "RESOURCE_EXHAUSTED", Reason: "input_limit_exceeded", Param: field, Message: message, Limit: &limit}
}
