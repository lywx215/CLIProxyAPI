package executor

const OperationMetadataKey = "operation"

// GeminiEnterpriseOperationAllowed deliberately requires an explicit native operation.
// Translated legacy endpoints must not acquire new provider capabilities by accident.
func GeminiEnterpriseOperationAllowed(source, response, operation string) bool {
	if source != response {
		return false
	}
	return (source == "openai" && operation == "chat_completions") ||
		(source == "gemini" && (operation == "generate_content" || operation == "stream_generate_content"))
}

func Operation(opts Options) string { op, _ := opts.Metadata[OperationMetadataKey].(string); return op }
