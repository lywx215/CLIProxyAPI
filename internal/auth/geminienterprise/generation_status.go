package geminienterprise

// generationStatus accepts canonical RPC codes and their REST HTTP equivalents.
// Conflicting code/name pairs remain unknown instead of changing retry behavior.
func generationStatus(code int, name string) (int, string) {
	for _, candidate := range []struct {
		rpc, http int
		name      string
	}{
		{1, 499, "CANCELLED"}, {2, 500, "UNKNOWN"},
		{3, 400, "INVALID_ARGUMENT"}, {4, 504, "DEADLINE_EXCEEDED"},
		{5, 404, "NOT_FOUND"}, {6, 409, "ALREADY_EXISTS"},
		{7, 403, "PERMISSION_DENIED"}, {8, 429, "RESOURCE_EXHAUSTED"},
		{9, 400, "FAILED_PRECONDITION"}, {10, 409, "ABORTED"},
		{11, 400, "OUT_OF_RANGE"}, {12, 501, "UNIMPLEMENTED"},
		{13, 500, "INTERNAL"}, {14, 503, "UNAVAILABLE"},
		{15, 500, "DATA_LOSS"}, {16, 401, "UNAUTHENTICATED"},
	} {
		matches := code == candidate.rpc || code == candidate.http
		if (code == 0 && name == "") || code != 0 && !matches || name != "" && name != candidate.name {
			continue
		}
		if name == "" && code == candidate.http {
			return candidate.http, ""
		}
		return candidate.http, candidate.name
	}
	return 0, ""
}
