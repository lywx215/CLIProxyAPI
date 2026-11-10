package handlers

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type observationFailureWriter struct {
	*httptest.ResponseRecorder
	short, flush bool
}

func (w *observationFailureWriter) Write(b []byte) (int, error) {
	if w.short {
		return 0, nil
	}
	return 0, io.ErrClosedPipe
}
func (w *observationFailureWriter) FlushError() error {
	if w.flush {
		return io.ErrClosedPipe
	}
	return nil
}

func TestObservedDeliveryCapturesEveryWriteAndFlush(t *testing.T) {
	for _, mode := range []string{"initial", "terminal", "heartbeat", "short_write", "flush"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(usage.WithObservedHTTPDelivery(t.Context()))
			defer cancel()
			stop := usage.SupportObservedStreamDelivery(ctx)
			defer stop()
			raw := &observationFailureWriter{ResponseRecorder: httptest.NewRecorder(), short: mode == "short_write", flush: mode == "flush"}
			c, _ := gin.CreateTestContext(raw)
			w := &observedDeliveryWriter{ResponseWriter: c.Writer, ctx: ctx, cancel: cancel}
			if mode == "flush" {
				_ = w.FlushError()
			} else {
				_, _ = w.WriteString("data: " + mode + "\n\n")
			}
			want := io.ErrClosedPipe
			if mode == "short_write" {
				want = io.ErrShortWrite
			}
			if ctx.Err() == nil || !errors.Is(w.writeError(), want) {
				t.Fatalf("write not settled: %v / %v", ctx.Err(), w.writeError())
			}
			if e, tracked := usage.WaitStreamDelivery(ctx); !tracked || !errors.Is(e, want) {
				t.Fatalf("ACK = %v / %v", e, tracked)
			}
		})
	}
}

func TestObservedDeliveryLeavesLegacyProducerPassive(t *testing.T) {
	ctx, cancel := context.WithCancel(usage.WithObservedHTTPDelivery(t.Context()))
	defer cancel()
	c, _ := gin.CreateTestContext(&observationFailureWriter{ResponseRecorder: httptest.NewRecorder()})
	w := &observedDeliveryWriter{ResponseWriter: c.Writer, ctx: ctx, cancel: cancel}
	_, _ = w.Write([]byte("legacy"))
	if ctx.Err() != nil || w.writeError() != nil || usage.StreamDeliveryTracked(ctx) {
		t.Fatal("legacy provider was opted into delivery tracking")
	}
}
