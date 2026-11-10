package handlers

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// observedDeliveryWriter only changes error handling for opted-in producers.
// It also covers the first chunk, terminal errors, and heartbeat writes.
type observedDeliveryWriter struct {
	gin.ResponseWriter
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
}

func (w *observedDeliveryWriter) record(err error) {
	if err == nil || !usage.ObservedDeliverySupported(w.ctx) {
		return
	}
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	first := w.err
	w.mu.Unlock()
	usage.FinishObservedDelivery(w.ctx, first)
	w.cancel()
}

func (w *observedDeliveryWriter) writeError() error { w.mu.Lock(); defer w.mu.Unlock(); return w.err }
func (w *observedDeliveryWriter) Write(p []byte) (int, error) {
	n, e := w.ResponseWriter.Write(p)
	if n < len(p) && e == nil {
		e = io.ErrShortWrite
	}
	w.record(e)
	return n, e
}
func (w *observedDeliveryWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *observedDeliveryWriter) Unwrap() http.ResponseWriter       { return w.ResponseWriter }
func (w *observedDeliveryWriter) Flush()                            { _ = w.FlushError() }
func (w *observedDeliveryWriter) FlushError() error {
	if !usage.ObservedDeliverySupported(w.ctx) {
		w.ResponseWriter.Flush()
		return nil
	}
	w.ResponseWriter.WriteHeaderNow()
	var writer http.ResponseWriter = w.ResponseWriter
	// Gin's void Flush hides errors. Unwrap it before asking ResponseController.
	for i := 0; i < 16; i++ {
		u, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		next := u.Unwrap()
		if next == nil || next == writer {
			break
		}
		writer = next
	}
	e := http.NewResponseController(writer).Flush()
	w.record(e)
	return e
}
