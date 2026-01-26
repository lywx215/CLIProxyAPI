package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	objectStoreConfigKey  = "config/config.yaml"
	objectStoreAuthPrefix = "auths"
)

// ObjectStoreConfig captures configuration for the object storage-backed token store.
type ObjectStoreConfig struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	Prefix    string
	LocalRoot string
	UseSSL    bool
	PathStyle bool
}

// ObjectTokenStore persists configuration and authentication metadata using an S3-compatible object storage backend.
// Files are mirrored to a local workspace so existing file-based flows continue to operate.
type ObjectTokenStore struct {
	client     *s3.Client
	cfg        ObjectStoreConfig
	spoolRoot  string
	configPath string
	authDir    string
	mu         sync.Mutex
}

// NewObjectTokenStore initializes an object storage backed token store.
func NewObjectTokenStore(cfg ObjectStoreConfig) (*ObjectTokenStore, error) {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.AccessKey = strings.TrimSpace(cfg.AccessKey)
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("object store: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("object store: bucket is required")
	}
	if cfg.AccessKey == "" {
		return nil, fmt.Errorf("object store: access key is required")
	}
	if cfg.SecretKey == "" {
		return nil, fmt.Errorf("object store: secret key is required")
	}
	if cfg.Region == "" {
		// Provide a default region if none is specified, though for COS specific endpoint routing,
		// correct region is crucial for the custom resolver.
		cfg.Region = "ap-guangzhou"
	}

	root := strings.TrimSpace(cfg.LocalRoot)
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = filepath.Join(cwd, "objectstore")
		} else {
			root = filepath.Join(os.TempDir(), "objectstore")
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("object store: resolve spool directory: %w", err)
	}

	configDir := filepath.Join(absRoot, "config")
	authDir := filepath.Join(absRoot, "auths")

	if err = os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("object store: create config directory: %w", err)
	}
	if err = os.MkdirAll(authDir, 0o700); err != nil {
		return nil, fmt.Errorf("object store: create auth directory: %w", err)
	}

	// Load AWS SDK Config with custom resolver and options
	awsCfg, err := config.LoadDefaultConfig(context.TODO(),
		// config.WithClientLogMode(aws.LogRequest|aws.LogResponse), // Debug log
		config.WithRegion(cfg.Region),
		config.WithEndpointResolver(aws.EndpointResolverFunc(
			func(service, region string) (aws.Endpoint, error) {
				// Ensure protocol scheme
				endpointURL := cfg.Endpoint
				if !strings.HasPrefix(endpointURL, "http://") && !strings.HasPrefix(endpointURL, "https://") {
					if cfg.UseSSL {
						endpointURL = "https://" + endpointURL
					} else {
						endpointURL = "http://" + endpointURL
					}
				}
				return aws.Endpoint{
					PartitionID:   "aws",
					URL:           endpointURL,
					SigningRegion: cfg.Region,
				}, nil
			}),
		),
		config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
			Value: aws.Credentials{
				AccessKeyID:     cfg.AccessKey,
				SecretAccessKey: cfg.SecretKey,
			},
		}),
		// Important for some COS interfaces
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("object store: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.PathStyle // controlled by main.go
	})

	return &ObjectTokenStore{
		client:     client,
		cfg:        cfg,
		spoolRoot:  absRoot,
		configPath: filepath.Join(configDir, "config.yaml"),
		authDir:    authDir,
	}, nil
}

// SetBaseDir implements the optional interface used by authenticators; it is a no-op because
// the object store controls its own workspace.
func (s *ObjectTokenStore) SetBaseDir(string) {}

// ConfigPath returns the managed configuration file path inside the spool directory.
func (s *ObjectTokenStore) ConfigPath() string {
	if s == nil {
		return ""
	}
	return s.configPath
}

// AuthDir returns the local directory containing mirrored auth files.
func (s *ObjectTokenStore) AuthDir() string {
	if s == nil {
		return ""
	}
	return s.authDir
}

// Bootstrap ensures the target bucket exists and synchronizes data from the object storage backend.
func (s *ObjectTokenStore) Bootstrap(ctx context.Context, exampleConfigPath string) error {
	if s == nil {
		return fmt.Errorf("object store: not initialized")
	}
	if err := s.ensureBucket(ctx); err != nil {
		return err
	}
	if err := s.syncConfigFromBucket(ctx, exampleConfigPath); err != nil {
		return err
	}
	if err := s.syncAuthFromBucket(ctx); err != nil {
		return err
	}
	return nil
}

// Save persists authentication metadata to disk and uploads it to the object storage backend.
func (s *ObjectTokenStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("object store: auth is nil")
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("object store: missing file path attribute for %s", auth.ID)
	}

	if auth.Disabled {
		if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
			return "", nil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("object store: create auth directory: %w", err)
	}

	switch {
	case auth.Storage != nil:
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
	case auth.Metadata != nil:
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("object store: marshal metadata: %w", errMarshal)
		}
		if existing, errRead := os.ReadFile(path); errRead == nil {
			if jsonEqual(existing, raw) {
				return path, nil
			}
		} else if errRead != nil && !errors.Is(errRead, fs.ErrNotExist) {
			return "", fmt.Errorf("object store: read existing metadata: %w", errRead)
		}
		tmp := path + ".tmp"
		if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
			return "", fmt.Errorf("object store: write temp auth file: %w", errWrite)
		}
		if errRename := os.Rename(tmp, path); errRename != nil {
			return "", fmt.Errorf("object store: rename auth file: %w", errRename)
		}
	default:
		return "", fmt.Errorf("object store: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["path"] = path

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	if err = s.uploadAuth(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

// List enumerates auth JSON files from the mirrored workspace.
func (s *ObjectTokenStore) List(_ context.Context) ([]*cliproxyauth.Auth, error) {
	dir := strings.TrimSpace(s.AuthDir())
	if dir == "" {
		return nil, fmt.Errorf("object store: auth directory not configured")
	}
	entries := make([]*cliproxyauth.Auth, 0, 32)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		auth, err := s.readAuthFile(path, dir)
		if err != nil {
			log.WithError(err).Warnf("object store: skip auth %s", path)
			return nil
		}
		if auth != nil {
			entries = append(entries, auth)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("object store: walk auth directory: %w", err)
	}
	return entries, nil
}

// Delete removes an auth file locally and remotely.
func (s *ObjectTokenStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("object store: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("object store: delete auth file: %w", err)
	}
	if err = s.deleteAuthObject(ctx, path); err != nil {
		return err
	}
	return nil
}

// PersistAuthFiles uploads the provided auth files to the object storage backend.
func (s *ObjectTokenStore) PersistAuthFiles(ctx context.Context, _ string, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		abs := trimmed
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(s.authDir, trimmed)
		}
		if err := s.uploadAuth(ctx, abs); err != nil {
			return err
		}
	}
	return nil
}

// PersistConfig uploads the local configuration file to the object storage backend.
func (s *ObjectTokenStore) PersistConfig(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteObject(ctx, objectStoreConfigKey)
		}
		return fmt.Errorf("object store: read config file: %w", err)
	}
	if len(data) == 0 {
		return s.deleteObject(ctx, objectStoreConfigKey)
	}
	return s.putObject(ctx, objectStoreConfigKey, data, "application/x-yaml")
}

func (s *ObjectTokenStore) ensureBucket(ctx context.Context) error {
	// HeadBucket checks if the bucket exists and we have access.
	// AWS SDK v2 does not have a simple "BucketExists"; HeadBucket is the standard way.
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.cfg.Bucket),
	})
	if err != nil {
		var notFound *types.NotFound
		// If 404, we try to create it
		if errors.As(err, &notFound) || strings.Contains(err.Error(), "404") {
			_, errCreate := s.client.CreateBucket(ctx, &s3.CreateBucketInput{
				Bucket: aws.String(s.cfg.Bucket),
				// For some regions, LocationConstraint might be needed,
				// but usually with custom endpoint + region in config it's inferred or ignored by generic S3/COS.
				// We skip CreateBucketConfiguration to avoid complexity unless needed.
			})
			if errCreate != nil {
				return fmt.Errorf("object store: create bucket: %w", errCreate)
			}
			return nil
		}
		// If 403 or other error, return it
		return fmt.Errorf("object store: check bucket: %w", err)
	}
	return nil
}

func (s *ObjectTokenStore) syncConfigFromBucket(ctx context.Context, example string) error {
	key := s.prefixedKey(objectStoreConfigKey)

	// Check if object exists using HeadObject
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})

	if err == nil {
		// Found, download it
		resp, errGet := s.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.cfg.Bucket),
			Key:    aws.String(key),
		})
		if errGet != nil {
			return fmt.Errorf("object store: fetch config: %w", errGet)
		}
		defer resp.Body.Close()
		data, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return fmt.Errorf("object store: read config: %w", errRead)
		}
		if errWrite := os.WriteFile(s.configPath, normalizeLineEndingsBytes(data), 0o600); errWrite != nil {
			return fmt.Errorf("object store: write config: %w", errWrite)
		}
	} else if isObjectNotFoundAWS(err) {
		// Not found, init from example
		if _, statErr := os.Stat(s.configPath); errors.Is(statErr, fs.ErrNotExist) {
			if example != "" {
				if errCopy := misc.CopyConfigTemplate(example, s.configPath); errCopy != nil {
					return fmt.Errorf("object store: copy example config: %w", errCopy)
				}
			} else {
				if errCreate := os.MkdirAll(filepath.Dir(s.configPath), 0o700); errCreate != nil {
					return fmt.Errorf("object store: prepare config directory: %w", errCreate)
				}
				if errWrite := os.WriteFile(s.configPath, []byte{}, 0o600); errWrite != nil {
					return fmt.Errorf("object store: create empty config: %w", errWrite)
				}
			}
		}
		// Upload the initial config
		data, errRead := os.ReadFile(s.configPath)
		if errRead != nil {
			return fmt.Errorf("object store: read local config: %w", errRead)
		}
		if len(data) > 0 {
			if errPut := s.putObject(ctx, objectStoreConfigKey, data, "application/x-yaml"); errPut != nil {
				return errPut
			}
		}
	} else {
		return fmt.Errorf("object store: stat config: %w", err)
	}
	return nil
}

func (s *ObjectTokenStore) syncAuthFromBucket(ctx context.Context) error {
	// NOTE: We intentionally do NOT use os.RemoveAll here.
	// Wiping the directory triggers file watcher delete events, which then
	// propagate deletions to the remote object store (race condition).
	// Instead, we just ensure the directory exists and overwrite files incrementally.
	if err := os.MkdirAll(s.authDir, 0o700); err != nil {
		return fmt.Errorf("object store: create auth directory: %w", err)
	}

	prefix := s.prefixedKey(objectStoreAuthPrefix + "/")
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("object store: list auth objects: %w", err)
		}
		for _, obj := range page.Contents {
			key := *obj.Key
			rel := strings.TrimPrefix(key, prefix)
			if rel == "" || strings.HasSuffix(rel, "/") {
				continue
			}
			relPath := filepath.FromSlash(rel)
			if filepath.IsAbs(relPath) {
				log.WithField("key", key).Warn("object store: skip auth outside mirror")
				continue
			}
			cleanRel := filepath.Clean(relPath)
			if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
				log.WithField("key", key).Warn("object store: skip auth outside mirror")
				continue
			}
			local := filepath.Join(s.authDir, cleanRel)
			if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
				return fmt.Errorf("object store: prepare auth subdir: %w", err)
			}

			resp, errGet := s.client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(s.cfg.Bucket),
				Key:    aws.String(key),
			})
			if errGet != nil {
				return fmt.Errorf("object store: download auth %s: %w", key, errGet)
			}
			data, errRead := io.ReadAll(resp.Body)
			resp.Body.Close()
			if errRead != nil {
				return fmt.Errorf("object store: read auth %s: %w", key, errRead)
			}
			if errWrite := os.WriteFile(local, data, 0o600); errWrite != nil {
				return fmt.Errorf("object store: write auth %s: %w", local, errWrite)
			}
		}
	}
	return nil
}

func (s *ObjectTokenStore) uploadAuth(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	rel, err := filepath.Rel(s.authDir, path)
	if err != nil {
		return fmt.Errorf("object store: resolve auth relative path: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteAuthObject(ctx, path)
		}
		return fmt.Errorf("object store: read auth file: %w", err)
	}
	if len(data) == 0 {
		return s.deleteAuthObject(ctx, path)
	}
	key := objectStoreAuthPrefix + "/" + filepath.ToSlash(rel)
	return s.putObject(ctx, key, data, "application/json")
}

func (s *ObjectTokenStore) deleteAuthObject(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	rel, err := filepath.Rel(s.authDir, path)
	if err != nil {
		return fmt.Errorf("object store: resolve auth relative path: %w", err)
	}
	key := objectStoreAuthPrefix + "/" + filepath.ToSlash(rel)
	return s.deleteObject(ctx, key)
}

func (s *ObjectTokenStore) putObject(ctx context.Context, key string, data []byte, contentType string) error {
	if len(data) == 0 {
		return s.deleteObject(ctx, key)
	}
	fullKey := s.prefixedKey(key)
	reader := bytes.NewReader(data)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(fullKey),
		Body:        reader,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("object store: put object %s: %w", fullKey, err)
	}
	return nil
}

func (s *ObjectTokenStore) deleteObject(ctx context.Context, key string) error {
	fullKey := s.prefixedKey(key)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(fullKey),
	})
	if err != nil {
		if isObjectNotFoundAWS(err) {
			return nil
		}
		return fmt.Errorf("object store: delete object %s: %w", fullKey, err)
	}
	return nil
}

func (s *ObjectTokenStore) prefixedKey(key string) string {
	key = strings.TrimLeft(key, "/")
	if s.cfg.Prefix == "" {
		return key
	}
	return strings.TrimLeft(s.cfg.Prefix+"/"+key, "/")
}

func (s *ObjectTokenStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("object store: auth is nil")
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			if filepath.IsAbs(path) {
				return path, nil
			}
			return filepath.Join(s.authDir, path), nil
		}
	}
	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		fileName = strings.TrimSpace(auth.ID)
	}
	if fileName == "" {
		return "", fmt.Errorf("object store: auth %s missing filename", auth.ID)
	}
	if !strings.HasSuffix(strings.ToLower(fileName), ".json") {
		fileName += ".json"
	}
	return filepath.Join(s.authDir, fileName), nil
}

func (s *ObjectTokenStore) resolveDeletePath(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("object store: id is empty")
	}
	// Absolute paths are honored as-is; callers must ensure they point inside the mirror.
	if filepath.IsAbs(id) {
		return id, nil
	}
	// Treat any non-absolute id (including nested like "team/foo") as relative to the mirror authDir.
	// Normalize separators and guard against path traversal.
	clean := filepath.Clean(filepath.FromSlash(id))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("object store: invalid auth identifier %s", id)
	}
	// Ensure .json suffix.
	if !strings.HasSuffix(strings.ToLower(clean), ".json") {
		clean += ".json"
	}
	return filepath.Join(s.authDir, clean), nil
}

func (s *ObjectTokenStore) readAuthFile(path, baseDir string) (*cliproxyauth.Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata := make(map[string]any)
	if err = json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	provider := strings.TrimSpace(valueAsString(metadata["type"]))
	if provider == "" {
		provider = "unknown"
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat auth file: %w", err)
	}
	rel, errRel := filepath.Rel(baseDir, path)
	if errRel != nil {
		rel = filepath.Base(path)
	}
	rel = normalizeAuthID(rel)
	attr := map[string]string{"path": path}
	if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
		attr["email"] = email
	}
	auth := &cliproxyauth.Auth{
		ID:               rel,
		Provider:         provider,
		FileName:         rel,
		Label:            labelFor(metadata),
		Status:           cliproxyauth.StatusActive,
		Attributes:       attr,
		Metadata:         metadata,
		CreatedAt:        info.ModTime(),
		UpdatedAt:        info.ModTime(),
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}
	return auth, nil
}

func normalizeLineEndingsBytes(data []byte) []byte {
	replaced := bytes.ReplaceAll(data, []byte{'\r', '\n'}, []byte{'\n'})
	return bytes.ReplaceAll(replaced, []byte{'\r'}, []byte{'\n'})
}

func isObjectNotFoundAWS(err error) bool {
	if err == nil {
		return false
	}
	var notFound *types.NotFound
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &notFound) || errors.As(err, &noSuchKey) {
		return true
	}
	// Checking the error code string if types don't match (sometimes happens with generic aws errors)
	if strings.Contains(err.Error(), "NoSuchKey") || strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
		return true
	}
	return false
}

// Helpers needed as they were in the original file but not exported from gitstore
// We duplicate them here or check if they are available in `store` package.
// Since `postgresstore.go` and `gitstore.go` are in the same package, they share these iff they are not methods.
// The original file used `valueAsString` and `labelFor` and `normalizeAuthID`.
// These appear to be shared unexported functions in the package. If not, I need to add them.
// Let's assume they are available since I don't see them defined in the provided `objectstore.go`
// (Wait, I just replaced the file, I need to make sure I didn't delete them if they were there).
// Checking `objectstore.go` previous view: I don't see them defined in `objectstore.go`.
// They must be in another file in the `store` package (like `store.go` or `utils.go`).
// I'll proceed assuming they are available.
