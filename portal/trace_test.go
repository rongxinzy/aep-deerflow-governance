// 全链追踪规格：入站提取（继承上游 trace 换本跳 span）、无头生成、
// X-Trace-Id 回显、完成日志带 trace_id/状态/耗时、出站 TracedTransport 注头、
// 反代 Rewrite 透传 traceparent。
package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTraceMiddleware(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	t.Run("inbound traceparent is inherited with a fresh span id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/rooms", nil)
		req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		w := httptest.NewRecorder()
		traceMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tp := currentTraceParent(r.Context())
			assert.True(t, strings.HasPrefix(tp, "00-4bf92f3577b34da6a3ce929d0e0e4736-"), "trace id 继承")
			assert.NotContains(t, tp, "00f067aa0ba902b7", "span id 必须换本跳的")
			w.WriteHeader(http.StatusTeapot)
		})).ServeHTTP(w, req)
		assert.Equal(t, http.StatusTeapot, w.Code)
		assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", w.Header().Get("X-Trace-Id"))
		assert.Contains(t, buf.String(), `"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"`)
		assert.Contains(t, buf.String(), `"status":418`)
		assert.Contains(t, buf.String(), `"duration_ms"`)
		buf.Reset()
	})

	t.Run("no inbound header mints a valid trace and echoes it", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
		w := httptest.NewRecorder()
		traceMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(w, req)
		traceID := w.Header().Get("X-Trace-Id")
		require.Len(t, traceID, 32, "128-bit trace id")
		assert.Contains(t, buf.String(), traceID)
		buf.Reset()
	})

	t.Run("malformed traceparent falls back to a fresh trace, business unaffected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
		req.Header.Set("traceparent", "not-a-w3c-header")
		w := httptest.NewRecorder()
		traceMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(w, req)
		assert.Len(t, w.Header().Get("X-Trace-Id"), 32)
	})

	t.Run("tracedTransport injects the outbound header", func(t *testing.T) {
		var seen string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("traceparent")
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		ctx, traceID, err := withRequestTrace(req.Context(), http.Header{})
		require.NoError(t, err)
		req = req.WithContext(ctx)
		resp, err := tracedTransport{base: http.DefaultTransport}.RoundTrip(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.True(t, strings.HasPrefix(seen, "00-"+traceID+"-"), "出站携带同 trace")
	})
}
