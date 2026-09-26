// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	pkgotel "latere.ai/x/pkg/otel"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// telemetry is an in-memory OpenTelemetry pipeline: a manual meter
// reader and a span recorder behind the global providers, which the
// public handler's otelhttp layer reads when it is built. The previous
// providers come back when the test ends.
type telemetry struct {
	reader *sdkmetric.ManualReader
	spans  *tracetest.SpanRecorder
}

func installTelemetry(t *testing.T) *telemetry {
	t.Helper()
	prevMeter, prevTracer := otel.GetMeterProvider(), otel.GetTracerProvider()
	tel := &telemetry{reader: sdkmetric.NewManualReader(), spans: tracetest.NewSpanRecorder()}
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(tel.reader)))
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(tel.spans)))
	t.Cleanup(func() {
		otel.SetMeterProvider(prevMeter)
		otel.SetTracerProvider(prevTracer)
	})
	return tel
}

// durations is the count of http.server.request.duration points per
// http.route, summed over the other attributes. A point without the
// attribute counts under "".
func (tel *telemetry) durations(t *testing.T) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tel.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("http.server.request.duration is a %T", m.Data)
			}
			for _, dp := range h.DataPoints {
				route, _ := dp.Attributes.Value(attribute.Key("http.route"))
				out[route.AsString()] += dp.Count
			}
		}
	}
	return out
}

// serverSpans are the ended server spans, the root span of every
// request the public handler observed.
func (tel *telemetry) serverSpans() []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range tel.spans.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			out = append(out, s)
		}
	}
	return out
}

// requestSeries is origo_requests_total per route, summed over the
// status classes: the series the metrics hook feeds.
var requestSeries = regexp.MustCompile(`(?m)^origo_requests_total\{route="([^"]*)",status_class="[^"]*"\} (\S+)$`)

func hookCounts(t *testing.T, internal string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, m := range requestSeries.FindAllStringSubmatch(scrape(t, internal), -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("origo_requests_total %q: %v", m[2], err)
		}
		out[m[1]] += v
	}
	return out
}

// loggedRoute is the route field of the request line whose trace id is
// traceID, and whether the node has written that line yet.
func loggedRoute(t *testing.T, logs *lockedBuffer, traceID string) (string, bool) {
	t.Helper()
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Msg     string `json:"msg"`
			Route   string `json:"route"`
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec.Msg == "request" && rec.TraceID == traceID {
			return rec.Route, true
		}
	}
	return "", false
}

// grown names the keys whose value rose between two snapshots.
func grown[V uint64 | float64](before, after map[string]V) []string {
	var out []string
	for k, v := range after {
		if v > before[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// TestRequestsAreNamedByRoute is the route vocabulary of the public
// listener held through the real handler: every request, git smart
// HTTP in both URL forms and both spellings, LFS, the API, and the
// named routes of the public mux, lands on one
// http.server.request.duration point whose http.route is the route's
// name and never a path, its root span is named by the method and the
// same route, and the metrics hook and the log line record it under
// the same name, so the Prometheus series, the log, and the
// OpenTelemetry signals agree. A request no handler serves carries no
// http.route, its span is named by the method alone, and the hook and
// the log line record it as pkg/otel's UnmatchedRoute.
func TestRequestsAreNamedByRoute(t *testing.T) {
	tel := installTelemetry(t)
	env, id := servingEnv(t)
	var logs lockedBuffer
	n, stop := startNodeLogging(t, env, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer func() { _ = stop() }()
	public, internal, _ := n.addrs()
	base := "http://" + public
	token := fixture(t, base, id)

	// The fixture is a create, a push and a clone in the label form, and
	// a refused push to a repository that does not exist: the smart HTTP
	// operations a git client makes, each under its own name.
	got := tel.durations(t)
	for _, want := range []string{
		"/v1/repos",
		"/{owner}/{slug}/info/refs?service=git-receive-pack",
		"/{owner}/{slug}/git-receive-pack",
		"/{owner}/{slug}/info/refs?service=git-upload-pack",
		"/{owner}/{slug}/git-upload-pack",
	} {
		if got[want] == 0 {
			t.Errorf("no http.server.request.duration point for %s; the routes are %v", want, slices.Sorted(maps.Keys(got)))
		}
	}

	client := &http.Client{
		Transport:     &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	label := "/" + repoOwner + "/" + repoSlug
	for _, row := range []struct {
		method, path, body, route string
	}{
		// The named routes of the public mux keep their patterns.
		{"GET", "/version", "", "/version"},
		{"GET", "/readyz", "", "/readyz"},
		{"GET", "/.well-known/jwks.json", "", "/.well-known/jwks.json"},
		{"GET", "/openapi.yaml", "", "/openapi.yaml"},
		{"GET", "/", "", "/{$}"},
		{"GET", "/favicon.ico", "", "/favicon.ico"},
		// Smart HTTP in the label form, with and without .git.
		{"GET", label + ".git/info/refs?service=git-upload-pack", "", "/{owner}/{slug}/info/refs?service=git-upload-pack"},
		{"GET", label + "/info/refs?service=git-receive-pack", "", "/{owner}/{slug}/info/refs?service=git-receive-pack"},
		{"GET", label + ".git/info/refs", "", "/{owner}/{slug}/info/refs"},
		{"GET", label + ".git/info/refs?service=other", "", "/{owner}/{slug}/info/refs"},
		{"POST", label + ".git/git-upload-pack", "0000", "/{owner}/{slug}/git-upload-pack"},
		{"POST", label + "/git-receive-pack", "0000", "/{owner}/{slug}/git-receive-pack"},
		// Smart HTTP in the id form, with and without .git.
		{"GET", "/r/" + repoID + ".git/info/refs?service=git-upload-pack", "", "/r/{id}/info/refs?service=git-upload-pack"},
		{"GET", "/r/" + repoID + "/info/refs?service=git-receive-pack", "", "/r/{id}/info/refs?service=git-receive-pack"},
		{"POST", "/r/" + repoID + ".git/git-upload-pack", "0000", "/r/{id}/git-upload-pack"},
		{"POST", "/r/" + repoID + "/git-receive-pack", "0000", "/r/{id}/git-receive-pack"},
		// LFS and the API are named by their own patterns.
		{"POST", label + ".git/info/lfs/objects/batch", `{"operation":"download","objects":[]}`, "/{owner}/{slug}/info/lfs/objects/batch"},
		{"GET", "/r/" + repoID + ".git/info/lfs/locks", "", "/r/{id}/info/lfs/locks"},
		{"GET", "/v1/repos/" + repoID, "", "/v1/repos/{id}"},
		{"GET", "/v1/repos/" + repoID + "/refs", "", "/v1/repos/{id}/refs"},
		// What no handler serves has no route, whatever the path holds.
		{"GET", label + ".git/git-upload-pack", "", ""},
		{"POST", label + ".git/info/refs", "", ""},
		{"GET", label + ".git/objects/info/packs", "", ""},
		{"GET", "/nothing", "", ""},
		{"GET", label, "", ""},
		{"POST", "/version", "", ""},
		{"PUT", "/v1/repos/" + repoID, "", ""},
	} {
		t.Run(row.method+" "+row.path, func(t *testing.T) {
			// The Prometheus label and the log field need a value where
			// the OpenTelemetry signals carry none.
			wantLabel, wantSpan := row.route, row.method+" "+row.route
			if row.route == "" {
				wantLabel, wantSpan = pkgotel.UnmatchedRoute, row.method
			}
			beforeOTel, beforeHook, beforeSpans := tel.durations(t), hookCounts(t, internal), len(tel.serverSpans())
			var body io.Reader
			if row.body != "" {
				body = strings.NewReader(row.body)
			}
			req, err := http.NewRequestWithContext(t.Context(), row.method, base+row.path, body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			traceID := resp.Header.Get("X-Trace-Id")
			if traceID == "" {
				t.Fatal("the response carries no X-Trace-Id")
			}

			// The handler records after it writes the response, so the
			// point can trail the client by a moment.
			var otelRoutes, hookRoutes []string
			var spans []sdktrace.ReadOnlySpan
			var logRoute string
			var logged bool
			for deadline := time.Now().Add(5 * time.Second); ; {
				otelRoutes = grown(beforeOTel, tel.durations(t))
				hookRoutes = grown(beforeHook, hookCounts(t, internal))
				spans = tel.serverSpans()[beforeSpans:]
				logRoute, logged = loggedRoute(t, &logs, traceID)
				if (len(otelRoutes) > 0 && len(hookRoutes) > 0 && len(spans) > 0 && logged) || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			// A point without http.route counts under "".
			if !slices.Equal(otelRoutes, []string{row.route}) {
				t.Errorf("http.server.request.duration grew under %q, want %q (status %d)", otelRoutes, row.route, resp.StatusCode)
			}
			if !slices.Equal(hookRoutes, []string{wantLabel}) {
				t.Errorf("origo_requests_total grew under %q, want %q", hookRoutes, wantLabel)
			}
			if !logged {
				t.Errorf("no request line carries trace id %s", traceID)
			} else if logRoute != wantLabel {
				t.Errorf("request line route %q, want %q", logRoute, wantLabel)
			}
			if len(spans) != 1 {
				t.Fatalf("%d server spans ended", len(spans))
			}
			if name := spans[0].Name(); name != wantSpan {
				t.Errorf("span name %q, want %q", name, wantSpan)
			}
			spanRoute, hasRoute := "", false
			for _, kv := range spans[0].Attributes() {
				if kv.Key == "http.route" {
					spanRoute, hasRoute = kv.Value.AsString(), true
				}
			}
			if hasRoute != (row.route != "") || spanRoute != row.route {
				t.Errorf("span http.route %q (set %v), want %q", spanRoute, hasRoute, row.route)
			}
		})
	}

	// No route name carries what a request named: not a repository id,
	// not an owner, not a slug.
	for route := range tel.durations(t) {
		for _, bad := range []string{repoID, repoOwner, repoSlug, "nope", "nothing"} {
			if strings.Contains(route, bad) {
				t.Errorf("http.route %q carries %q", route, bad)
			}
		}
		if route == "/" {
			t.Errorf("a request was recorded under http.route %q", route)
		}
	}
}
