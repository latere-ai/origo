// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// recording installs a tracer provider that keeps every span in memory
// and restores the previous one when the test ends.
func recording(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func TestStartNestsSpansAndCarriesAttributes(t *testing.T) {
	rec := recording(t)
	ctx, end := Start(context.Background(), "receive", Repo("r1"), Subject("alice"), Actor(""))
	if ID(ctx) == "" {
		t.Fatal("the context carries no trace id")
	}
	child, endChild := Start(ctx, "entry.put", Phase("entry"))
	if ID(child) != ID(ctx) {
		t.Fatalf("child trace id %q, parent %q", ID(child), ID(ctx))
	}
	Set(child, Actor("bob"))
	endChild()
	end()

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	if spans[0].Name() != "entry.put" || spans[1].Name() != "receive" {
		t.Fatalf("names %q, %q", spans[0].Name(), spans[1].Name())
	}
	if spans[0].Parent().SpanID() != spans[1].SpanContext().SpanID() {
		t.Fatal("entry.put is not a child of receive")
	}
	got := map[string]string{}
	for _, kv := range spans[1].Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got["origo.repo"] != "r1" || got["origo.subject"] != "alice" {
		t.Fatalf("attributes %v", got)
	}
	if _, ok := got["origo.actor"]; ok {
		t.Fatalf("an empty attribute was recorded: %v", got)
	}
	child0 := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		child0[string(kv.Key)] = kv.Value.AsString()
	}
	if child0["origo.phase"] != "entry" || child0["origo.actor"] != "bob" {
		t.Fatalf("child attributes %v", child0)
	}
}

// TestUnderTheNoopProviderNothingIsRecorded is the production default:
// without OTEL_EXPORTER_OTLP_ENDPOINT the global provider is the noop
// one, and every function here still answers.
func TestUnderTheNoopProviderNothingIsRecorded(t *testing.T) {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	ctx, end := Start(context.Background(), "receive", Repo("r1"))
	defer end()
	if id := ID(ctx); id != "" {
		t.Fatalf("trace id %q without a provider", id)
	}
	Set(ctx, Subject("alice")) // must not panic
	if id := ID(context.Background()); id != "" {
		t.Fatalf("a bare context carries the trace id %q", id)
	}
	Set(context.Background())
}
