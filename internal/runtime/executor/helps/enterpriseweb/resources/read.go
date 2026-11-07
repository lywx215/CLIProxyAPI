// Adapted from lywx215/AIStudio2API (MIT, Copyright 2026 Mag1cFall).
package resources

import (
	"context"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/enterpriseweb/contract"
)

const readChunkBytes = 64 << 10

// ReadAll reserves each input chunk before allocating it and reserves the
// final contiguous copy before constructing it. It never trusts a declared
// Content-Length. Only the returned backing buffer stays charged on success.
func ReadAll(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, contract.Fail(500, "INTERNAL", "Invalid Business body limit")
	}
	var chunks [][]byte
	var chunkReleases, resultReleases []func()
	total := int64(0)
	success := false
	defer func() {
		for _, release := range chunkReleases {
			release()
		}
		if !success {
			for _, release := range resultReleases {
				release()
			}
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, ContextError(err)
		}
		capacity := int(min(int64(readChunkBytes), limit-total+1))
		if capacity <= 0 {
			return nil, bodyTooLarge()
		}
		release, err := Reserve(ctx, int64(capacity))
		if err != nil {
			return nil, err
		}
		buffer := make([]byte, capacity)
		chunkReleases = append(chunkReleases, release)
		count, readErr := io.ReadFull(reader, buffer)
		if err := ctx.Err(); err != nil {
			return nil, ContextError(err)
		}
		if int64(count) > limit-total {
			return nil, bodyTooLarge()
		}
		if count > 0 {
			copyRelease, err := Reserve(ctx, int64(count))
			if err != nil {
				return nil, err
			}
			resultReleases = append(resultReleases, copyRelease)
			chunks = append(chunks, buffer[:count])
			total += int64(count)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	result := make([]byte, int(total))
	offset := 0
	for _, chunk := range chunks {
		offset += copy(result[offset:], chunk)
	}
	success = true
	return result, nil
}

func bodyTooLarge() error {
	return contract.Fail(413, "RESOURCE_EXHAUSTED", "Business content exceeds the per-request size limit")
}
