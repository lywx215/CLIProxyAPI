package diagnostics

import (
	"bufio"
	"net"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
)

var routePattern = regexp.MustCompile(`\A/[A-Za-z0-9_:/{}.*-]*\z`)

// GinMiddleware runs outside recovery so recovered error responses use the same
// commit boundary. requestID returns the existing local ID, never a caller ID.
func (e *Engine) GinMiddleware(requestID func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, span := e.StartServer(c.Request.Context(), c.Request.Header, requestID(c))
		if span == nil {
			c.Next()
			return
		}
		c.Request = c.Request.WithContext(ctx)
		w := &responseWriter{ResponseWriter: c.Writer, span: span}
		c.Writer = w
		completed := false
		defer func() {
			// Gin commits a status-only response after handlers return. Prepare its
			// IDs here but never force WriteHeaderNow or invent a committed wire status.
			w.prepare()
			data := ServerData{HeadersCommitted: w.Written(), EndReason: "finished", DeliveryState: "local_finished"}
			if data.HeadersCommitted {
				status := w.Status()
				data.WireStatus = &status
			}
			if route := c.FullPath(); len(route) <= 128 && routePattern.MatchString(route) {
				data.RouteTemplate = &route
			}
			if !completed || w.failed {
				data.EndReason, data.DeliveryState = "error", "failed"
			}
			if ctx.Err() != nil {
				data.EndReason, data.DeliveryState = "client_cancel", "cancelled"
			}
			if w.hijacked || !w.Written() {
				data.DeliveryState = "unknown"
			}
			if w.hijacked {
				// Gin marks its writer as written during Hijack, before any raw
				// handshake bytes. Its cached status is not the wire status.
				data.HeadersCommitted, data.WireStatus = false, nil
				data.EndReason = "unknown"
			}
			span.Finish(data)
		}()
		c.Next()
		completed = true
	}
}

type responseWriter struct {
	gin.ResponseWriter
	span             *ServerSpan
	failed, hijacked bool
}

func (w *responseWriter) prepare() {
	if !w.Written() && !w.hijacked {
		ResponseHeaders(w.Header(), w.span.requestID, w.span.incoming.TraceID)
	}
}
func (w *responseWriter) WriteHeader(code int) { w.prepare(); w.ResponseWriter.WriteHeader(code) }
func (w *responseWriter) WriteHeaderNow()      { w.prepare(); w.ResponseWriter.WriteHeaderNow() }
func (w *responseWriter) Write(p []byte) (int, error) {
	w.prepare()
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.failed = true
	}
	return n, err
}
func (w *responseWriter) WriteString(s string) (int, error) {
	w.prepare()
	n, err := w.ResponseWriter.WriteString(s)
	if err != nil {
		w.failed = true
	}
	return n, err
}
func (w *responseWriter) Flush() { w.prepare(); w.ResponseWriter.Flush() }
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
