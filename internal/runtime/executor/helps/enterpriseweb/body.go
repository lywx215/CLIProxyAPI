package enterpriseweb

import (
	"bytes"
	"context"
	"io"
	"os"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
	webgemini "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/gemini"
)

func HasMedia(b []byte) bool {
	if !bytes.Contains(b, []byte("inlineData")) && !bytes.Contains(b, []byte("inline_data")) && !bytes.Contains(b, []byte("fileData")) && !bytes.Contains(b, []byte("file_data")) && !bytes.Contains(b, []byte(`\u`)) {
		return false
	}
	var scanner KeyScanner
	// Classification must not disappear when history exceeds parser limits.
	// Full validation runs after admission, before any upstream operation.
	return scanner.Feed(b)
}

// HasMediaInContext reuses HTTP admission when the immutable body is unchanged.
// Interceptor replacements are classified again rather than trusting stale input.
func HasMediaInContext(ctx context.Context, b []byte) bool {
	if cached, ok := Body(ctx); ok && len(cached) == len(b) && (len(b) == 0 || &cached[0] == &b[0]) {
		return ExistingLease(ctx) != nil
	}
	return HasMedia(b)
}
func ContentCount(b []byte) int { _, count := webgemini.InlineMediaInfo(b); return count }

// KeyScanner observes JSON string keys incrementally without buffering values.
type KeyScanner struct {
	in, escaped, after bool
	token              []byte
	key                byte
	frames             []scanFrame
	overflow           bool
}

type scanFrame struct{ role, key byte }

const (
	scanOther byte = iota
	scanRoot
	scanContents
	scanContent
	scanParts
	scanPart
)
const (
	keyOther byte = iota
	keyContents
	keyParts
	keyInline
)

func (s *KeyScanner) open(kind byte) {
	if len(s.frames) >= 10000 {
		s.overflow = true
		return
	}
	role := scanOther
	if len(s.frames) == 0 && kind == '{' {
		role = scanRoot
	} else if len(s.frames) > 0 {
		parent := s.frames[len(s.frames)-1]
		switch {
		case parent.role == scanRoot && parent.key == keyContents && kind == '[':
			role = scanContents
		case parent.role == scanContents && kind == '{':
			role = scanContent
		case parent.role == scanContent && parent.key == keyParts && kind == '[':
			role = scanParts
		case parent.role == scanParts && kind == '{':
			role = scanPart
		}
	}
	s.frames = append(s.frames, scanFrame{role: role})
}

func (s *KeyScanner) Feed(b []byte) bool {
	if s.overflow {
		return false
	}
	for len(b) > 0 {
		if s.in {
			if s.escaped {
				s.escaped = false
				if len(s.token) < 96 {
					s.token = append(s.token, b[0])
				}
				b = b[1:]
				continue
			}
			next := bytes.IndexAny(b, `"\`)
			if next < 0 {
				if len(s.token) < 96 {
					s.token = append(s.token, b[:min(len(b), 96-len(s.token))]...)
				}
				return false
			}
			if len(s.token) < 96 {
				s.token = append(s.token, b[:min(next, 96-len(s.token))]...)
			}
			value := b[next]
			b = b[next+1:]
			if value == '\\' {
				s.escaped = true
				if len(s.token) < 96 {
					s.token = append(s.token, value)
				}
				continue
			}
			s.in = false
			valueString, err := strconv.Unquote("\"" + string(s.token) + "\"")
			s.key = keyOther
			if err == nil {
				switch valueString {
				case "contents":
					s.key = keyContents
				case "parts":
					s.key = keyParts
				case "inlineData", "inline_data", "fileData", "file_data":
					s.key = keyInline
				}
			}
			s.after = true
			continue
		}
		value := b[0]
		b = b[1:]
		if s.after {
			if value == ' ' || value == '\r' || value == '\n' || value == '\t' {
				continue
			}
			if value == ':' && len(s.frames) > 0 {
				frame := &s.frames[len(s.frames)-1]
				frame.key = s.key
				if frame.role == scanPart && s.key == keyInline {
					return true
				}
			}
			s.after = false
		}
		switch value {
		case '"':
			s.in = true
			s.token = s.token[:0]
			continue
		case '{', '[':
			s.open(value)
			if s.overflow {
				return false
			}
		case '}', ']':
			if len(s.frames) > 0 {
				s.frames = s.frames[:len(s.frames)-1]
			}
		case ',':
			if len(s.frames) > 0 {
				s.frames[len(s.frames)-1].key = keyOther
			}
		}
		if next := bytes.IndexAny(b, "{}[],\""); next >= 0 {
			b = b[next:]
		} else {
			return false
		}
	}
	return false
}

// ReadBody admits media as soon as its key appears, before buffering its value.
func ReadBody(ctx context.Context, r io.Reader, length int64, rt *Runtime, beforeMedia ...func() error) ([]byte, context.Context, *Lease, error) {
	settings := rt.admissionConfig(ctx)
	ctx = context.WithValue(ctx, admissionConfigKey{}, settings)
	// Until media is classified, preserve the existing ordinary Enterprise limit.
	maxBodyBytes := max(MaxBodyBytes, settings.InputLimits.MaxRequestBytes)
	if length == 0 {
		length = -1
	}
	if length > maxBodyBytes {
		err := InputFailureFrom(contract.LimitFailure("body", "Media request exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitDeclaredRequestBytes, Observed: length, Maximum: maxBodyBytes}))
		observePreflight(ctx, "input_limit_exceeded", err)
		return nil, ctx, nil, err
	}
	var scan KeyScanner
	var lease *Lease
	var mediaSeen bool
	capacity := int64(4096)
	if length > 0 && length <= 64<<10 {
		capacity = length
	}
	body := make([]byte, 0, int(capacity))
	var scratch [4096]byte
	var spool *os.File
	var total int64
	defer func() {
		if spool != nil {
			_ = spool.Close()
			_ = os.Remove(spool.Name())
		}
	}()
	fail := func(err error) ([]byte, context.Context, *Lease, error) {
		if lease != nil {
			rt.RecordPreflight(ctx, err)
			lease.Release()
		} else if failure, ok := err.(*Failure); ok && failure.Code == 413 {
			if mediaSeen {
				rt.recordPreAdmissionFailure(ctx, "input_limit_exceeded", err)
			} else {
				observePreflight(ctx, "input_limit_exceeded", err)
			}
		}
		return nil, ctx, nil, err
	}
	for {
		n, e := r.Read(scratch[:])
		if n > 0 {
			total += int64(n)
			if length > 0 && total > length {
				return fail(Fail(400, "Request body exceeds its declared length"))
			}
			if total > maxBodyBytes {
				return fail(InputFailureFrom(contract.LimitFailure("body", "Request exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitRequestBytes, Observed: total, Maximum: maxBodyBytes, AtLeast: true})))
			}
			if lease == nil && scan.Feed(scratch[:n]) {
				mediaSeen = true
				if len(beforeMedia) > 0 && beforeMedia[0] != nil {
					if err := beforeMedia[0](); err != nil {
						return fail(err)
					}
				}
				maxBodyBytes = settings.InputLimits.MaxRequestBytes
				if length > maxBodyBytes {
					return fail(InputFailureFrom(contract.LimitFailure("body", "Media request exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitDeclaredRequestBytes, Observed: length, Maximum: maxBodyBytes})))
				}
				if total > maxBodyBytes {
					return fail(InputFailureFrom(contract.LimitFailure("body", "Media request exceeds its configured byte limit", contract.InputLimit{Name: contract.LimitRequestBytes, Observed: total, Maximum: maxBodyBytes, AtLeast: true})))
				}
				var err error
				ctx, lease, err = rt.Acquire(ctx, length)
				if err != nil {
					return fail(err)
				}
				capacity := length
				if capacity < total {
					capacity = maxBodyBytes
				}
				next := make([]byte, 0, int(capacity))
				if spool != nil {
					if _, err = spool.Seek(0, io.SeekStart); err != nil {
						return fail(err)
					}
					buf := bytes.NewBuffer(next)
					if _, err = io.Copy(buf, spool); err != nil {
						return fail(err)
					}
					next = buf.Bytes()
				} else {
					next = append(next, body...)
				}
				body = next
			}
			if scan.overflow {
				return fail(Fail(400, "JSON nesting exceeds its limit"))
			}
			// Keep the unclassified prefix bounded, even when text precedes the attachment.
			if lease == nil && (spool != nil || len(body)+n > 64<<10) {
				if spool == nil {
					var err error
					spool, err = os.CreateTemp("", "cpa-media-prefix-*")
					if err != nil {
						return fail(Fail(503, "Request spool is unavailable"))
					}
					if _, err = spool.Write(body); err != nil {
						return fail(err)
					}
					body = nil
				}
				if _, err := spool.Write(scratch[:n]); err != nil {
					return fail(err)
				}
			} else {
				body = append(body, scratch[:n]...)
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return fail(e)
		}
		if e = ctx.Err(); e != nil {
			return fail(e)
		}
	}
	if length > 0 && total != length {
		return fail(Fail(400, "Request body is shorter than its declared length"))
	}
	if lease == nil && total > MaxBodyBytes {
		return fail(InputFailureFrom(contract.LimitFailure("body", "Request exceeds its byte limit", contract.InputLimit{Name: contract.LimitRequestBytes, Observed: total, Maximum: MaxBodyBytes})))
	}
	if lease == nil && spool != nil {
		if _, e := spool.Seek(0, io.SeekStart); e != nil {
			return fail(e)
		}
		var e error
		body, e = io.ReadAll(spool)
		if e != nil {
			return fail(e)
		}
	}
	if lease != nil && !HasMedia(body) {
		return fail(Fail(400, "Inline media must belong to valid content parts"))
	}
	return body, ctx, lease, nil
}
