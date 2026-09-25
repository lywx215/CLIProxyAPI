package diagnostics

// CoverageEvidence describes bounded observations of an exported span. It is
// separate from collection switches: neither current DEBUG nor a terminal stub
// proves a complete lifetime. This helper is also used to validate local output.
type CoverageEvidence struct {
	Sequences          []uint64 `json:"sequences"`
	ExpectedLastLogSeq *uint64  `json:"expectedLastLogSeq"`
	TerminalCount      uint64   `json:"terminalCount"`
	TerminalStubCount  uint64   `json:"terminalStubCount"`
	TerminalLogSeq     *uint64  `json:"terminalLogSeq"`
	DebugCapture       string   `json:"debugCapture"`
	AccessCapture      string   `json:"accessCapture"`
	DroppedForSpan     *uint64  `json:"droppedForSpan"`
	TruncatedEvents    *uint64  `json:"truncatedEvents"`
	Conflict           bool     `json:"conflict"`
	ExportKnownLoss    bool     `json:"exportKnownLoss"`
}
type CoverageAssessment struct {
	DebugCoverage   string `json:"debugCoverage"`
	TerminalMissing bool   `json:"terminalMissing"`
}

func AssessCoverage(e CoverageEvidence) CoverageAssessment {
	out := CoverageAssessment{DebugCoverage: "unknown", TerminalMissing: e.TerminalCount == 0 && e.TerminalStubCount == 0}
	positive := func(p *uint64) bool { return p != nil && *p > 0 }
	gap := false
	if e.ExpectedLastLogSeq != nil {
		seen := make(map[uint64]struct{}, len(e.Sequences))
		for _, n := range e.Sequences {
			seen[n] = struct{}{}
			if n == 0 || n > *e.ExpectedLastLogSeq {
				gap = true
			}
		}
		// Avoid allocating/iterating a range controlled by an exported counter.
		gap = gap || uint64(len(seen)) != *e.ExpectedLastLogSeq
	}
	if e.DebugCapture == "interrupted" || e.AccessCapture == "interrupted" || positive(e.DroppedForSpan) || positive(e.TruncatedEvents) || e.TerminalStubCount > 0 || e.Conflict || e.ExportKnownLoss || gap {
		out.DebugCoverage = "partial"
	} else if e.TerminalCount != 1 || e.ExpectedLastLogSeq == nil || e.TerminalLogSeq == nil || *e.TerminalLogSeq != *e.ExpectedLastLogSeq || e.DebugCapture == "unknown" || e.AccessCapture == "none" || e.AccessCapture == "unknown" || e.DroppedForSpan == nil || e.TruncatedEvents == nil {
		out.DebugCoverage = "unknown"
	} else if e.DebugCapture == "none" {
		out.DebugCoverage = "none"
	} else if e.AccessCapture == "enabled_throughout" {
		out.DebugCoverage = "full"
	}
	return out
}
