// 全链追踪（W3C Trace Context）：portal 是浏览器链路的枢纽——在这里收/生
// traceparent、给每个请求打一条完成日志（状态/耗时/路径）、把 trace 注入
// 全部出站调用（AEP 控制面走 TracedTransport；网关反代在 Rewrite 里带
// 头），并把镜像 X-Trace-Id 回给调用方，报障时一眼可查。
//
// 只用 otel 的传播（格式解析/注入），不引 SDK/采集——这是日志关联型追踪，
// 不是 APM；trace id 出现在每一跳的日志里即可完成排查闭环。
package main

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var w3cPropagator = propagation.TraceContext{}

// currentTraceParent renders the W3C header for the span context in ctx, or ""
// when the request carried no trace (e.g. background jobs).
func currentTraceParent(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	carrier := propagation.HeaderCarrier(http.Header{})
	w3cPropagator.Inject(trace.ContextWithSpanContext(ctx, sc), carrier)
	return carrier.Get("traceparent")
}

func newSpanContext() (trace.SpanContext, error) {
	var traceBytes [16]byte
	var spanBytes [8]byte
	if _, err := rand.Read(traceBytes[:]); err != nil {
		return trace.SpanContext{}, err
	}
	if _, err := rand.Read(spanBytes[:]); err != nil {
		return trace.SpanContext{}, err
	}
	tid := trace.TraceID{}
	copy(tid[:], traceBytes[:])
	sid := trace.SpanID{}
	copy(sid[:], spanBytes[:])
	return trace.SpanContext{}.WithTraceID(tid).WithSpanID(sid).WithTraceFlags(trace.FlagsSampled), nil
}

// withRequestTrace extracts the inbound traceparent (W3C) or mints a fresh
// trace when absent/malformed, and stashes it in the context. Downstream code
// reads it via currentTraceParent.
func withRequestTrace(ctx context.Context, header http.Header) (context.Context, string, error) {
	extracted := w3cPropagator.Extract(ctx, propagation.HeaderCarrier(header))
	if sc := trace.SpanContextFromContext(extracted); sc.IsValid() {
		// 继承上游 trace，换本跳 span id（同一 trace 下逐跳可分）
		fresh, err := newSpanContext()
		if err != nil {
			return ctx, "", err
		}
		sc = sc.WithSpanID(fresh.SpanID())
		return trace.ContextWithSpanContext(ctx, sc), sc.TraceID().String(), nil
	}
	sc, err := newSpanContext()
	if err != nil {
		return ctx, "", err
	}
	return trace.ContextWithSpanContext(ctx, sc), sc.TraceID().String(), nil
}

// statusRecorder captures the response status for the completion log line.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// traceMiddleware wraps one handler: trace in/out, X-Trace-Id echo, and the
// per-request completion log（portal 此前每个请求零日志——排障只能靠猜）。
func traceMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/health" {
			next.ServeHTTP(w, r) // 探活不打点：每次探针铸新 trace 是纯噪音
			return
		}
		start := time.Now()
		ctx, traceID, err := withRequestTrace(r.Context(), r.Header)
		if err != nil {
			// 追踪失败不挡业务：无 trace 继续服务
			ctx = r.Context()
			traceID = ""
		}
		r = r.WithContext(ctx)
		if traceID != "" {
			w.Header().Set("X-Trace-Id", traceID)
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if traceID != "" {
			logger.Info("http request",
				"trace_id", traceID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}
	})
}

// tracedTransport injects the current traceparent onto every outbound request
// whose context carries a trace — one wrap covers all AEP admin calls without
// touching individual call sites.
type tracedTransport struct {
	base http.RoundTripper
}

func (t tracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if tp := currentTraceParent(req.Context()); tp != "" {
		req = req.Clone(req.Context())
		req.Header.Set("traceparent", tp)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
