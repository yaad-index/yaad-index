package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// requestIDHeader is the header name surfaced on every response so callers
// can correlate logs with their request. If the inbound request already
// carries this header, the same value is propagated (callers passing
// through a proxy or CLI client get end-to-end correlation); otherwise the
// middleware generates a fresh one.
const requestIDHeader = "X-Request-Id"

// requestIDKeyType keeps the context key collision-free without exporting
// a string. RequestIDFromContext is the only reader.
type requestIDKeyType struct{}

var requestIDKey requestIDKeyType

// RequestIDFromContext returns the per-request id stamped by withRequestID,
// or the empty string if the context wasn't passed through this middleware.
// Used by recover-side logging and would be used by handlers if they ever
// need to surface the id beyond the response header.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// withRequestID attaches a per-request id to the context and the response
// header. An inbound X-Request-Id is preserved if present (caller-side
// correlation); otherwise a 16-byte hex id is generated.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newRequestID returns a 32-character hex id from 16 cryptographic random
// bytes. Crypto-grade entropy is overkill for trace correlation; using it
// anyway because the read is cheap and removes any "is this id unique"
// question. Falls back to a timestamp+pid mash if rand.Reader fails — a
// near-impossible path in practice but still better than dropping the id.
func newRequestID() string {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("req-fallback-%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// statusCapturingResponseWriter records whether (and at what status) a
// response has been written. Recover-side logic uses `wroteHeader` to
// decide whether it can still emit a 500 envelope: if the handler already
// committed status + body before panicking, the connection is in an
// unrecoverable state and we just log the panic.
type statusCapturingResponseWriter struct {
	http.ResponseWriter
	status int
	wroteHeader bool
}

func (w *statusCapturingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		// Stdlib already logs a "superfluous WriteHeader" warning; just
		// ignore the duplicate call so the recorded status reflects the
		// first commit.
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusCapturingResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// withRecover catches panics from any downstream handler, logs them with
// the per-request id + stack, and emits a canonical 500 envelope when
// possible (i.e. the handler hadn't already written a response). This is
// the only place a panic can become an HTTP error envelope; the existing
// per-handler `writeError` calls remain the source of every other error
// shape (no double-wrapping).
func withRecover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := &statusCapturingResponseWriter{ResponseWriter: w}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				reqID := RequestIDFromContext(r.Context())
				logger.ErrorContext(r.Context(), "panic in handler",
					"request_id", reqID,
					"panic", fmt.Sprintf("%v", rec),
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				if !ww.wroteHeader {
					writeError(ww, http.StatusInternalServerError, "internal_error",
						"internal server error")
				}
				// If the handler had already committed a response when it
				// panicked, the connection is in an unrecoverable state —
				// no further bytes can be sent. Logged above; nothing more
				// to do here.
			}()
			next.ServeHTTP(ww, r)
		})
	}
}
