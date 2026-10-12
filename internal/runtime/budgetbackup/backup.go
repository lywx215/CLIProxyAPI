// Package budgetbackup makes low-frequency, single-attempt backups of the local Gemini ledger.
package budgetbackup

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
)

var (
	ErrUploadFailed = errors.New("Gemini budget backup upload failed; no retry will be made today")
	ErrRepeatedPUT  = errors.New("Gemini budget backup permits only one HTTP PUT attempt")
)

// Ledger must durably claim the Pacific day before returning true. A missing or
// damaged ledger must refuse claims until its disaster recovery freeze has ended.
type Ledger interface {
	ClaimBackup(context.Context, time.Time) (bool, error)
	ExportSnapshot(context.Context) (budget.Snapshot, error)
	SetBackupResult(context.Context, time.Time, bool, string) error
}

// Options configure a separate client; credentials are never copied into snapshots.
type Options struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	Prefix    string
	ObjectKey string
	Schedule  string
	PathStyle bool
}

// Writer performs exactly one PUT without retries, multipart uploads, or redirects.
type Writer interface {
	Put(context.Context, []byte) error
}

// Service has no startup, shutdown, or request-driven backup side effects.
type Service struct {
	ledger Ledger
	writer Writer
	hour   int
	minute int
	now    func() time.Time
	mu     sync.Mutex
}

// NewFromEnvironment reuses only the existing object-store connection parameters.
// It does not reuse ObjectTokenStore, its client, or its automatic sync paths.
func NewFromEnvironment(ctx context.Context, options Options, ledger Ledger) (*Service, error) {
	options.Endpoint = envDefault(options.Endpoint, "OBJECTSTORE_ENDPOINT")
	options.Bucket = envDefault(options.Bucket, "OBJECTSTORE_BUCKET")
	options.AccessKey = envDefault(options.AccessKey, "OBJECTSTORE_ACCESS_KEY")
	options.SecretKey = envDefault(options.SecretKey, "OBJECTSTORE_SECRET_KEY")
	options.Region = envDefault(options.Region, "OBJECTSTORE_REGION")
	options.Prefix = envDefault(options.Prefix, "OBJECTSTORE_PREFIX")
	return New(ctx, options, ledger)
}

func envDefault(value, name string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	if value = os.Getenv(name); strings.TrimSpace(value) != "" {
		return value
	}
	return os.Getenv(strings.ToLower(name))
}

// New constructs the independent, non-retrying S3 client without making a request.
func New(ctx context.Context, options Options, ledger Ledger) (*Service, error) {
	writer, err := newS3Writer(ctx, options)
	if err != nil {
		return nil, err
	}
	return NewWithWriter(options, ledger, writer)
}

// NewWithWriter allows a separately verified single-attempt writer to be supplied.
func NewWithWriter(options Options, ledger Ledger, writer Writer) (*Service, error) {
	if ledger == nil || writer == nil {
		return nil, errors.New("Gemini budget backup requires a ledger and writer")
	}
	hour, minute, err := parseSchedule(options.Schedule)
	if err != nil {
		return nil, err
	}
	return &Service{ledger: ledger, writer: writer, hour: hour, minute: minute, now: time.Now}, nil
}

// Backup is called only by the scheduled task or after explicit administrator init.
// False means no upload was performed: unchanged, already claimed, or frozen.
// True means the day's opportunity was durably consumed, even if networking failed.
func (s *Service) Backup(ctx context.Context) (bool, error) {
	return s.backupAt(ctx, s.now())
}

func (s *Service) backupAt(ctx context.Context, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	snapshot, err := s.ledger.ExportSnapshot(ctx)
	if err != nil {
		return false, err
	}
	if !snapshot.Dirty {
		return false, nil
	}
	claimed, err := s.ledger.ClaimBackup(ctx, now)
	if err != nil || !claimed {
		return false, err
	}
	// ClaimBackup fsyncs the daily marker before either serialization or networking.
	snapshot, err = s.ledger.ExportSnapshot(ctx)
	if err != nil {
		return true, s.finish(ctx, now, false, "snapshot unavailable", err)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return true, s.finish(ctx, now, false, "snapshot serialization failed", err)
	}
	// A slow dial, TLS handshake, serialization, or upload can cross midnight.
	// Every physical write claims any newly entered Pacific day before writing.
	// This conservatively consumes both days for an upload spanning midnight.
	var gateMu sync.Mutex
	claimedDays := map[string]time.Time{now.In(pacific).Format("2006-01-02"): now}
	gate := func() error {
		gateMu.Lock()
		defer gateMu.Unlock()
		current := s.now()
		day := current.In(pacific).Format("2006-01-02")
		if _, exists := claimedDays[day]; exists {
			return nil
		}
		allowed, errClaim := s.ledger.ClaimBackup(ctx, current)
		if errClaim != nil {
			return errClaim
		}
		if !allowed {
			return ErrRepeatedPUT
		}
		claimedDays[day] = current
		return nil
	}
	putCtx := context.WithValue(ctx, backupDayGateKey{}, gate)
	if err = s.writer.Put(putCtx, body); err != nil {
		// SDK errors may contain endpoint details or response bodies. Do not expose them.
		return true, s.finish(ctx, now, false, "upload failed; daily attempt consumed", ErrUploadFailed)
	}
	// Mark only the serialized snapshot's original claim revision as backed up.
	// A newer-day marker cannot mark intervening business changes as clean.
	gateMu.Lock()
	for day, at := range claimedDays {
		if day != now.In(pacific).Format("2006-01-02") {
			_ = s.ledger.SetBackupResult(context.WithoutCancel(ctx), at, false, "upload crossed Pacific midnight; earlier snapshot retained")
		}
	}
	gateMu.Unlock()
	return true, s.finish(ctx, now, true, "uploaded", nil)
}

func (s *Service) finish(ctx context.Context, now time.Time, success bool, message string, uploadErr error) error {
	// A cancelled network context must not prevent recording the local result.
	if err := s.ledger.SetBackupResult(context.WithoutCancel(ctx), now, success, message); err != nil {
		return errors.Join(uploadErr, errors.New("Gemini budget backup result could not be persisted"))
	}
	return uploadErr
}

// Run schedules only future Pacific wall-clock occurrences. Starting after the
// scheduled time never catches up or uploads immediately, and stopping never uploads.
func (s *Service) Run(ctx context.Context) {
	for {
		next := nextScheduled(s.now(), s.hour, s.minute)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// Clock changes and suspended hosts must not turn a missed day into a catch-up.
			now := s.now()
			if now.Before(next) || !sameScheduledMinute(now, next) {
				continue
			}
			_, _ = s.backupAt(ctx, now)
		}
	}
}

func parseSchedule(value string) (int, int, error) {
	if value == "" {
		value = "00:10"
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, 0, errors.New("Gemini budget backup schedule must be HH:MM")
	}
	hour, errHour := strconv.Atoi(parts[0])
	minute, errMinute := strconv.Atoi(parts[1])
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, errors.New("Gemini budget backup schedule must be HH:MM")
	}
	return hour, minute, nil
}

var pacific, _ = time.LoadLocation("America/Los_Angeles")

func nextScheduled(now time.Time, hour, minute int) time.Time {
	local := now.In(pacific)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, pacific)
	if !next.After(now) {
		day := local.AddDate(0, 0, 1)
		next = time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, pacific)
	}
	return next
}

func sameScheduledMinute(a, b time.Time) bool {
	a, b = a.In(pacific), b.In(pacific)
	return a.Format("2006-01-02 15:04") == b.Format("2006-01-02 15:04")
}

type s3Writer struct {
	client *s3.Client
	bucket string
	key    string
}

func newS3Writer(ctx context.Context, options Options) (*s3Writer, error) {
	endpoint := strings.TrimSpace(options.Endpoint)
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Gemini budget backup requires a valid object-store endpoint")
	}
	if strings.TrimSpace(options.Bucket) == "" || strings.TrimSpace(options.AccessKey) == "" || strings.TrimSpace(options.SecretKey) == "" {
		return nil, errors.New("Gemini budget backup object-store connection parameters are incomplete")
	}
	if options.Region == "" {
		options.Region = "ap-guangzhou"
	}
	if options.ObjectKey == "" {
		options.ObjectKey = "quota/gemini-budget/latest.json"
	}
	key := options.ObjectKey
	if strings.HasPrefix(key, "/") || strings.Contains(key, "..") || path.Clean(key) != key || key == "." || strings.HasPrefix(key, "auths/") || strings.HasPrefix(key, "config/") {
		return nil, errors.New("Gemini budget backup object key must be an independent relative object key")
	}
	if prefix := strings.Trim(options.Prefix, "/"); prefix != "" {
		if path.Clean(prefix) != prefix || prefix == "." || prefix == ".." || strings.HasPrefix(prefix, "../") {
			return nil, errors.New("Gemini budget backup object-store prefix is invalid")
		}
		key = prefix + "/" + key
	}
	// A new HTTP/1 connection per PUT prevents Transport's reused-connection and
	// HTTP/2 stream retries from bypassing the SDK retry policy.
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
		TLSClientConfig:   &tls.Config{NextProtos: []string{"http/1.1"}},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, errDial := (&net.Dialer{}).DialContext(ctx, network, address)
			if errDial != nil {
				return nil, errDial
			}
			gate, _ := ctx.Value(backupDayGateKey{}).(func() error)
			return &backupDayConn{Conn: conn, gate: gate}, nil
		},
	}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	client := &http.Client{
		Transport: onePUTTransport{inner: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(options.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(options.AccessKey, options.SecretKey, "")),
		awsconfig.WithRetryer(func() aws.Retryer { return aws.NopRetryer{} }),
		awsconfig.WithHTTPClient(client),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
	)
	if err != nil {
		return nil, errors.New("Gemini budget backup S3 client initialization failed")
	}
	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(strings.TrimRight(endpoint, "/"))
		o.UsePathStyle = options.PathStyle
		o.Retryer = aws.NopRetryer{}
	})
	return &s3Writer{client: s3Client, bucket: options.Bucket, key: key}, nil
}

type putPermitKey struct{}
type backupDayGateKey struct{}

type backupDayConn struct {
	net.Conn
	gate func() error
}

func (c *backupDayConn) Write(p []byte) (int, error) {
	if c.gate != nil {
		if err := c.gate(); err != nil {
			_ = c.Conn.Close()
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

type onePUTTransport struct{ inner http.RoundTripper }

func (t onePUTTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	permit, ok := req.Context().Value(putPermitKey{}).(*atomic.Bool)
	if req.Method != http.MethodPut || !ok || !permit.CompareAndSwap(false, true) {
		return nil, ErrRepeatedPUT
	}
	return t.inner.RoundTrip(req)
}

func (w *s3Writer) Put(ctx context.Context, body []byte) error {
	ctx = context.WithValue(ctx, putPermitKey{}, &atomic.Bool{})
	_, err := w.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(w.bucket), Key: aws.String(w.key),
		Body: bytes.NewReader(body), ContentType: aws.String("application/json"),
	})
	if err != nil {
		return ErrUploadFailed
	}
	return nil
}
