package diagnosticanalyzer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
)

const (
	MaxRecordBytes = 4096
	MaxSources     = 128
	MaxBytes       = 128 << 20
	MaxLines       = 250000
	MaxRecords     = 20000
	MaxIssues      = 1000
)

// Input trust is supplied by the operator, never decoded from a log or bundle.
// Alias must be a deliberately chosen, nonsensitive token. Size is optional
// filesystem evidence, not a completeness assertion. Reader remains caller-owned.
type Input struct {
	Alias     string
	Reader    io.Reader
	Size      *int64
	Trusted   bool
	KnownLoss bool
}

type Limits struct {
	Bytes   int64 `json:"bytes"`
	Lines   int   `json:"lines"`
	Records int   `json:"records"`
	Issues  int   `json:"issues"`
}

func DefaultLimits() Limits { return Limits{MaxBytes, MaxLines, MaxRecords, MaxIssues} }

type Provenance struct {
	Source int   `json:"source"`
	Line   int   `json:"line"`
	Bytes  int64 `json:"bytes"`
}

type Issue struct {
	Provenance
	LengthComplete bool   `json:"lengthComplete"`
	Reason         string `json:"reason"`
}

type Resource struct {
	Environment  string `json:"environment"`
	DeploymentID string `json:"deploymentId"`
	Service      string `json:"service"`
	InstanceID   string `json:"instanceId"`
	BootID       string `json:"bootId"`
}

func resource(r object) Resource {
	return Resource{str(r["environment"]), str(r["deploymentId"]), str(r["service"]), str(r["instanceId"]), str(r["bootId"])}
}
func (r Resource) key() string {
	return r.Environment + "|" + r.DeploymentID + "|" + r.Service + "|" + r.InstanceID + "|" + r.BootID
}

type Source struct {
	ID           int        `json:"id"`
	Alias        string     `json:"alias"`
	Trusted      bool       `json:"trusted"`
	KnownLoss    bool       `json:"knownLoss"`
	Size         *int64     `json:"fileSize"`
	ScannedBytes int64      `json:"scannedBytes"`
	Lines        int        `json:"lines"`
	Accepted     int        `json:"accepted"`
	Quarantined  int        `json:"quarantined"`
	LegacyLines  int        `json:"legacyLines"`
	CompleteScan bool       `json:"completeScan"`
	SHA256       string     `json:"sha256,omitempty"`
	Resources    []Resource `json:"resources"`
	Findings     []string   `json:"findings"`
}

// Evidence retains every distinct schema-valid payload and every import location.
// Conflicting variants share Identity but never replace one another.
type Evidence struct {
	ID         int          `json:"id"`
	Identity   string       `json:"identity"`
	Record     object       `json:"record"`
	Provenance []Provenance `json:"provenance"`
	Trusted    bool         `json:"trusted"`
	Conflict   bool         `json:"conflict"`
}

type dataset struct {
	sources       []Source
	events        []*Evidence
	issues        []Issue
	issuesOmitted int
	limited       bool
	limits        Limits
}

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func readInputs(inputs []Input, limits Limits) (*dataset, error) {
	if len(inputs) == 0 || len(inputs) > MaxSources {
		return nil, errors.New("invalid_source_count")
	}
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if limits.Bytes < 1 || limits.Bytes > MaxBytes || limits.Lines < 1 || limits.Lines > MaxLines || limits.Records < 1 || limits.Records > MaxRecords || limits.Issues < 1 || limits.Issues > MaxIssues {
		return nil, errors.New("invalid_limits")
	}
	aliases := map[string]bool{}
	for _, in := range inputs {
		if !aliasPattern.MatchString(in.Alias) || aliases[in.Alias] || in.Reader == nil || in.Size != nil && *in.Size < 0 {
			return nil, errors.New("invalid_source_config")
		}
		aliases[in.Alias] = true
	}
	schema, reason := decodeUnique(recordSchema)
	if reason != "" {
		return nil, errors.New("embedded_schema_invalid")
	}
	validate, err := compileSchema(schema, obj(schema))
	if err != nil {
		return nil, err
	}
	d := &dataset{limits: limits, sources: []Source{}, events: []*Evidence{}, issues: []Issue{}}
	identities := map[string][]*Evidence{}
	same := map[string]*Evidence{}
	var totalBytes int64
	totalLines, accepted := 0, 0
	for i, in := range inputs {
		s := Source{ID: i + 1, Alias: in.Alias, Trusted: in.Trusted, KnownLoss: in.KnownLoss, Size: in.Size, Resources: []Resource{}, Findings: []string{}}
		resources := map[string]bool{}
		hash := sha256.New()
		reader := bufio.NewReaderSize(in.Reader, 8192)
		issue := func(p Provenance, reason string, complete bool) {
			s.Quarantined++
			if len(d.issues) < limits.Issues {
				d.issues = append(d.issues, Issue{p, complete, reason})
			} else {
				d.issuesOmitted++
			}
		}
		for {
			if totalBytes >= limits.Bytes || totalLines >= limits.Lines || accepted >= limits.Records {
				d.limited = true
				s.Findings = append(s.Findings, "scan_limit")
				break
			}
			// Accumulate only a bounded prefix; drain a huge line in fixed chunks.
			line := make([]byte, 0, MaxRecordBytes)
			var length int64
			complete, ended, readFailed := false, false, false
			for {
				remaining := limits.Bytes - totalBytes
				if remaining == 0 {
					break
				}
				// The reader may prefetch <=8192 bytes, but no data past the
				// reported scan boundary participates in evidence or hashing.
				part, errRead := reader.ReadSlice('\n')
				if int64(len(part)) > remaining {
					part = part[:remaining]
					errRead = bufio.ErrBufferFull
				}
				_, _ = hash.Write(part)
				totalBytes += int64(len(part))
				s.ScannedBytes += int64(len(part))
				length += int64(len(part))
				if len(line) < MaxRecordBytes {
					n := min(len(part), MaxRecordBytes-len(line))
					line = append(line, part[:n]...)
				}
				if errRead == nil {
					complete = true
					break
				}
				if errRead == io.EOF {
					complete = true
					ended = true
					break
				}
				if errRead != bufio.ErrBufferFull {
					readFailed = true
					break
				}
			}
			if length == 0 && ended {
				s.CompleteScan = true
				break
			}
			if length > 0 {
				s.Lines++
				totalLines++
			}
			p := Provenance{s.ID, s.Lines, length}
			if !complete {
				if length == 0 {
					p.Line++
				}
				if readFailed {
					s.Findings = append(s.Findings, "read_error")
					issue(p, "read_error", false)
				} else {
					d.limited = true
					s.Findings = append(s.Findings, "scan_limit")
					issue(p, "scan_limit", false)
				}
				break
			}
			if length > MaxRecordBytes {
				issue(p, "line_limit", true)
			} else if len(bytes.TrimSpace(line)) > 0 {
				payload := bytes.TrimSuffix(line, []byte("\n"))
				payload = bytes.TrimSuffix(payload, []byte("\r"))
				if bytes.HasPrefix(payload, []byte("@diag ")) {
					payload = payload[6:]
				} else if len(bytes.TrimSpace(payload)) == 0 || bytes.TrimSpace(payload)[0] != '{' {
					s.LegacyLines++
					if ended {
						s.CompleteScan = true
					}
					if ended {
						break
					}
					continue
				}
				v, why := decodeUnique(payload)
				r := obj(v)
				if why == "" && r != nil && r["diagnosticSchema"] != nil && r["diagnosticSchema"] != SchemaVersion {
					why = "unsupported_version"
				}
				if why == "" && !validate(v) {
					why = "schema_invalid"
				}
				if why == "" && len(semanticIssues(r)) > 0 {
					why = "semantic_invalid"
				}
				if why != "" {
					issue(p, why, true)
				} else {
					accepted++
					s.Accepted++
					res := resource(r)
					if !resources[res.key()] {
						resources[res.key()] = true
						s.Resources = append(s.Resources, res)
					}
					identity := str(r["service"]) + "|" + str(r["instanceId"]) + "|" + str(r["bootId"]) + "|" + str(r["spanId"]) + "|" + canonical(r["logSeq"])
					key := identity + "|" + canonical(r)
					if e := same[key]; e != nil {
						e.Provenance = append(e.Provenance, p)
						e.Trusted = e.Trusted && in.Trusted
					} else {
						e := &Evidence{ID: len(d.events) + 1, Identity: identity, Record: r, Provenance: []Provenance{p}, Trusted: in.Trusted}
						if len(identities[identity]) > 0 {
							e.Conflict = true
							// Older variants were marked when the second arrived.
							identities[identity][0].Conflict = true
						}
						identities[identity] = append(identities[identity], e)
						same[key] = e
						d.events = append(d.events, e)
					}
				}
			}
			if ended {
				s.CompleteScan = true
				break
			}
		}
		if s.CompleteScan {
			s.SHA256 = hex.EncodeToString(hash.Sum(nil))
			if s.Size != nil && *s.Size != s.ScannedBytes {
				s.CompleteScan = false
				s.Findings = append(s.Findings, "size_changed")
			}
		}
		for _, r := range s.Resources {
			if r.Service == "gcli2api" && s.Size != nil && *s.Size >= 16777216-4096 {
				s.Findings = append(s.Findings, "suspected_tail_gap_near_producer_limit")
				break
			}
		}
		if s.LegacyLines > 0 {
			s.Findings = append(s.Findings, "legacy_text_omitted")
		}
		if s.Quarantined > 0 {
			s.Findings = append(s.Findings, "quarantined_lines")
		}
		if s.KnownLoss {
			s.Findings = append(s.Findings, "export_known_loss")
		}
		d.sources = append(d.sources, s)
	}
	return d, nil
}
