// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package tracing starts the spans of spec 011 and is the one package in
// Origo that reaches the OpenTelemetry SDK.
//
// latere.ai/x/pkg/otel bootstraps the exporters, wraps a handler, and
// wraps a transport, but exposes no tracer, so a child span needs the
// SDK itself. Confining that import here keeps the rest of the module on
// the standard library and pkg, and keeps the arrival of the SDK on the
// node's build list one decision recorded in .lateregate.yaml.
//
// Without OTEL_EXPORTER_OTLP_ENDPOINT the global provider is the noop
// one: Start returns the context it was given and a function that does
// nothing, and ID answers the empty string.
package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// scope names the instrumentation library on every span Origo creates.
const scope = "github.com/latere-ai/origo"

// Attr is one span attribute. Repository id, subject, and actor travel
// here and never on a metric label (spec 011).
type Attr struct {
	Key   string
	Value string
}

// Repo, Subject, and Actor are the attributes spec 011 names.
func Repo(id string) Attr   { return Attr{Key: "origo.repo", Value: id} }
func Subject(s string) Attr { return Attr{Key: "origo.subject", Value: s} }
func Actor(a string) Attr   { return Attr{Key: "origo.actor", Value: a} }
func Phase(p string) Attr   { return Attr{Key: "origo.phase", Value: p} }

// Start opens a child span of the span on ctx and returns the context
// that carries it with the function that ends it. The end function is
// always non-nil, so a caller defers it without a check.
func Start(ctx context.Context, name string, attrs ...Attr) (context.Context, func()) {
	ctx, span := otel.Tracer(scope).Start(ctx, name, trace.WithAttributes(keyValues(attrs)...))
	return ctx, func() { span.End() }
}

// Set adds attributes to the span already on ctx. A request's repository
// and identity are known after the handler has resolved them, which is
// after the span began.
func Set(ctx context.Context, attrs ...Attr) {
	if len(attrs) == 0 {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return
	}
	span.SetAttributes(keyValues(attrs)...)
}

// ID reports the trace id of the span on ctx, or "" when ctx carries no
// sampled span. It is what a request names itself by: the log line's
// trace_id and the LFS body's request_id (spec 010).
func ID(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return ""
	}
	return span.SpanContext().TraceID().String()
}

// keyValues drops an attribute with an empty value: a span attribute
// that says nothing costs a backend the same as one that does.
func keyValues(attrs []Attr) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		if a.Value == "" {
			continue
		}
		out = append(out, attribute.String(a.Key, a.Value))
	}
	return out
}
