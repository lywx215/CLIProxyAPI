package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var (
	errObjectMissing        = errors.New("object not found")
	errObjectChanged        = errors.New("object write condition failed")
	errObjectOutcomeUnknown = errors.New("object write outcome unknown")
	objectErrorCode         = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,79}$`)
)

func objectHTTPStatus(err error) int {
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		return response.HTTPStatusCode()
	}
	return 0
}

func (s *ObjectTokenStore) getVersionedObject(ctx context.Context, key string, limit int64) (body []byte, etag string, err error) {
	response, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.prefixedKey(key)),
	})
	if err != nil {
		var missing *types.NoSuchKey
		var api interface{ ErrorCode() string }
		if errors.As(err, &missing) || (objectHTTPStatus(err) == http.StatusNotFound &&
			errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound")) {
			return nil, "", errObjectMissing
		}
		return nil, "", safeObjectRequestError("get object", err)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("close object response: %w", errClose))
		}
	}()
	etag = aws.ToString(response.ETag)
	if !validObjectETag(etag) {
		return nil, "", errors.New("object response lacks a valid ETag")
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", safeObjectRequestError("read object", err)
	}
	if int64(len(body)) > limit {
		return nil, "", errors.New("object exceeds its size limit")
	}
	return body, etag, nil
}

func validObjectETag(etag string) bool {
	if len(etag) < 3 || len(etag) > 1024 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		return false
	}
	for _, char := range etag[1 : len(etag)-1] {
		if char <= ' ' || char == '"' || char == 0x7f {
			return false
		}
	}
	return true
}

// An empty expected ETag means create-if-absent. Existing objects must use the
// exact validator captured from a read of the body used to make the change.
func (s *ObjectTokenStore) putVersionedObject(ctx context.Context, key string, body []byte, etag string) (string, error) {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(s.prefixedKey(key)),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	}
	if etag == "" {
		input.IfNoneMatch = aws.String("*")
	} else if validObjectETag(etag) {
		input.IfMatch = aws.String(etag)
	} else {
		return "", errObjectChanged
	}
	response, err := s.client.PutObject(ctx, input, noObjectWriteRetry)
	if err != nil {
		return "", classifyObjectWrite(err)
	}
	newETag := aws.ToString(response.ETag)
	if !validObjectETag(newETag) {
		return "", fmt.Errorf("%w: object response lacks a valid ETag", errObjectOutcomeUnknown)
	}
	return newETag, nil
}

func (s *ObjectTokenStore) deleteVersionedObject(ctx context.Context, key, etag string) error {
	if !validObjectETag(etag) {
		return errObjectChanged
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:  aws.String(s.cfg.Bucket),
		Key:     aws.String(s.prefixedKey(key)),
		IfMatch: aws.String(etag),
	}, noObjectWriteRetry)
	return classifyObjectWrite(err)
}

func noObjectWriteRetry(options *s3.Options) {
	options.Retryer = aws.NopRetryer{}
	options.RetryMaxAttempts = 1
}

func classifyObjectWrite(err error) error {
	if err == nil {
		return nil
	}
	status := objectHTTPStatus(err)
	switch status {
	case http.StatusPreconditionFailed, http.StatusConflict:
		return fmt.Errorf("%w (HTTP %d)", errObjectChanged, status)
	default:
		return fmt.Errorf("%w: %w", errObjectOutcomeUnknown, safeObjectRequestError("mutate object", err))
	}
}

func safeObjectRequestError(operation string, err error) error {
	var api interface{ ErrorCode() string }
	if errors.As(err, &api) && objectErrorCode.MatchString(api.ErrorCode()) {
		return fmt.Errorf("%s: HTTP %d (%s)", operation, objectHTTPStatus(err), api.ErrorCode())
	}
	return fmt.Errorf("%s: %T (HTTP %d)", operation, err, objectHTTPStatus(err))
}
