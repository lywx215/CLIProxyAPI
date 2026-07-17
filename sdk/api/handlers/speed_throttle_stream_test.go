package handlers

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

func TestSpeedThrottleForwardStreamDoesNotDelayFinalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/stream", nil)

	visible := []byte(`{"candidates":[{"content":{"parts":[{"text":"abcdefghijklmnop"}]}}]}`)
	usage := []byte(`{"usageMetadata":{"candidatesTokenCount":7000,"thoughtsTokenCount":3000}}`)
	data := make(chan []byte, 2)
	data <- visible
	data <- usage
	close(data)

	throttler := &RequestThrottler{
		targetRate:     1,
		startTime:      time.Now().Add(-10 * time.Second),
		totalTokens:    2,
		firstChunkSent: true,
	}
	handler := &BaseAPIHandler{}
	done := make(chan struct{})

	go func() {
		defer close(done)
		handler.ForwardStream(c, recorder, func(error) {}, data, (<-chan *interfaces.ErrorMessage)(nil), StreamForwardOptions{
			ThrottleDelay: func(chunk []byte) {
				throttler.ThrottleChunk(c.Request.Context(), chunk)
			},
			WriteChunk: func(chunk []byte) {
				_, _ = recorder.Write(chunk)
			},
		})
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ForwardStream did not finish promptly after final usage metadata")
	}

	if !bytes.Contains(recorder.Body.Bytes(), usage) {
		t.Fatal("final usage metadata was not forwarded")
	}
}
