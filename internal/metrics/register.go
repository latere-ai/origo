// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package metrics is the list of every metric Origo exposes and the one
// place they are registered.
//
// Spec 011 owns the names, with one exception it states: spec 024 owns
// the three series of the SSH listener in a table of its own. The table
// below is both tables in code:
// one row per metric with its type, its label vocabularies, and its
// buckets. Register walks it once at start-up, so GET /metrics carries
// every name from the first scrape, including the names of a spec that
// is not built yet, and hands the recording packages the handles they
// write through. Nothing outside this package registers a metric.
//
// The registry itself is latere.ai/x/pkg/metrics; this is the list of
// names over it.
package metrics

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	pkgmetrics "latere.ai/x/pkg/metrics"
)

// kind is what a row of the table becomes on the registry.
type kind int

const (
	counter kind = iota
	histogram
	gauge
)

// label is one label of a metric with the values it may take. An empty
// Values is an open vocabulary: the route of a request is the mux
// pattern, so no set of values can be seeded at start-up.
type label struct {
	Name   string
	Values []string
}

// metric is one row of spec 011's table.
type metric struct {
	Name    string
	Kind    kind
	Help    string
	Labels  []label
	Buckets []float64
}

// Duration bucket sets. The three histograms of the phase 1 packages
// keep the bounds they were recorded with, so a dashboard built on them
// keeps reading.
var (
	headCheckBuckets   = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}
	materializeBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60}
	pushBuckets        = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	compactionBuckets  = []float64{0.5, 1, 5, 10, 30, 60, 120, 300, 600}
)

// The label vocabularies. A value that is not here is never recorded, so
// a hostile client cannot grow the cardinality of a series.
var (
	headCheckResults  = []string{"404", "200", "error"}
	pushPhases        = []string{"receive", "entry", "index", "apply"}
	evictionReasons   = []string{"pressure", "idle"}
	gossipDirections  = []string{"sent", "received", "dropped"}
	compactionResults = []string{"ok", "stale", "error", "skipped"}
	authorizerResults = []string{"allow", "deny", "error"}
	rateLimits        = []string{"subject", "subprocesses", "repository"}
	storageOps        = []string{"get", "put", "create", "head", "delete", "list"}
	storageResults    = []string{"ok", "not_found", "exists", "error"}
	breakerClasses    = []string{"read", "write"}
	sshServices       = []string{"upload-pack", "receive-pack"}
	sshResults        = []string{"ok", "refused", "error"}
	sshAuthResults    = []string{"ok", "unknown_key", "resolver_error", "timeout"}
	sshKeyResults     = []string{"found", "not_found", "error"}
)

// table is spec 011's metric table. The order is the spec's.
var table = []metric{
	{Name: "origo_wal_commits_total", Kind: counter, Help: "index objects this node created"},
	{Name: "origo_wal_commit_conflicts_total", Kind: counter, Help: "commits refused because a reference moved"},
	{Name: "origo_wal_commit_retries_total", Kind: counter, Help: "commit rounds lost to another writer and replayed"},
	{Name: "origo_wal_entry_bytes_total", Kind: counter, Help: "bytes written as entries"},
	{Name: "origo_wal_head_check_seconds", Kind: histogram, Help: "latency of the HEAD currency check", Buckets: headCheckBuckets,
		Labels: []label{{Name: "result", Values: headCheckResults}}},
	{Name: "origo_pushes_total", Kind: counter, Help: "pushes acknowledged"},
	{Name: "origo_pushes_rejected_total", Kind: counter, Help: "pushes refused by the log"},
	{Name: "origo_fetches_total", Kind: counter, Help: "upload-pack requests served"},
	{Name: "origo_repo_materialized_total", Kind: counter, Help: "repositories built from the log onto an empty disk"},
	{Name: "origo_repo_entries_applied_total", Kind: counter, Help: "log entries applied to local copies"},
	{Name: "origo_repo_rebuilt_total", Kind: counter, Help: "local copies removed as corrupt and rebuilt"},
	{Name: "origo_repo_materialize_seconds", Kind: histogram, Help: "time to bring a local copy current", Buckets: materializeBuckets},
	{Name: "origo_requests_total", Kind: counter, Help: "requests served on the public listener",
		Labels: []label{{Name: "route"}, {Name: "status_class"}}},
	{Name: "origo_request_duration_seconds", Kind: histogram, Help: "time to serve a request on the public listener",
		Buckets: pkgmetrics.DefaultDurationBuckets, Labels: []label{{Name: "route"}, {Name: "status_class"}}},
	{Name: "origo_requests_in_flight", Kind: gauge, Help: "requests started and not finished on the public listener"},
	{Name: "origo_push_duration_seconds", Kind: histogram, Help: "time spent in each phase of a push", Buckets: pushBuckets,
		Labels: []label{{Name: "phase", Values: pushPhases}}},
	{Name: "origo_cache_bytes", Kind: gauge, Help: "bytes held by the local copies"},
	{Name: "origo_cache_repos", Kind: gauge, Help: "local copies held"},
	{Name: "origo_evictions_total", Kind: counter, Help: "local copies removed by reason",
		Labels: []label{{Name: "reason", Values: evictionReasons}}},
	{Name: "origo_gossip_packets_total", Kind: counter, Help: "gossip datagrams by direction",
		Labels: []label{{Name: "direction", Values: gossipDirections}}},
	{Name: "origo_compactions_total", Kind: counter, Help: "compaction runs by result",
		Labels: []label{{Name: "result", Values: compactionResults}}},
	{Name: "origo_compaction_seconds", Kind: histogram, Help: "time one compaction took", Buckets: compactionBuckets},
	{Name: "origo_authorizer_seconds", Kind: histogram, Help: "authorizer calls by result", Buckets: pkgmetrics.DefaultDurationBuckets,
		Labels: []label{{Name: "result", Values: authorizerResults}}},
	{Name: "origo_events_delivered_total", Kind: counter, Help: "events answered 2xx by the sink"},
	{Name: "origo_events_dead_total", Kind: counter, Help: "events moved to the dead-letter prefix after the window"},
	{Name: "origo_rate_limited_total", Kind: counter, Help: "requests refused by a limit",
		Labels: []label{{Name: "limit", Values: rateLimits}}},
	{Name: "origo_storage_ops_total", Kind: counter, Help: "object storage operations by result",
		Labels: []label{{Name: "op", Values: storageOps}, {Name: "result", Values: storageResults}}},
	{Name: "origo_storage_seconds", Kind: histogram, Help: "latency of one object storage operation",
		Buckets: pkgmetrics.DefaultDurationBuckets, Labels: []label{{Name: "op", Values: storageOps}}},
	{Name: "origo_storage_breaker_state", Kind: gauge, Help: "0 closed, 1 open, 2 half-open",
		Labels: []label{{Name: "class", Values: breakerClasses}}},
	{Name: "origo_stale_responses_total", Kind: counter, Help: "responses served from a copy that may be behind the log"},
	{Name: "origo_log_integrity_errors_total", Kind: counter, Help: "packs or entries the log names that are missing or fail their digest"},
	{Name: "origo_orphan_objects", Kind: gauge, Help: "objects under the prefix no index names"},
	{Name: "origo_storage_bytes", Kind: gauge, Help: "bytes under the prefix"},
	{Name: "origo_ssh_sessions_total", Kind: counter, Help: "SSH sessions by service and result",
		Labels: []label{{Name: "service", Values: sshServices}, {Name: "result", Values: sshResults}}},
	{Name: "origo_ssh_auth_total", Kind: counter, Help: "SSH authentication attempts by result",
		Labels: []label{{Name: "result", Values: sshAuthResults}}},
	{Name: "origo_ssh_keys_seconds", Kind: histogram, Help: "key resolver calls by result",
		Buckets: pkgmetrics.DefaultDurationBuckets, Labels: []label{{Name: "result", Values: sshKeyResults}}},
}

// Names reports every metric name of the table, in the table's order. It
// is what the presence test of spec 011 scrapes for.
func Names() []string {
	out := make([]string, 0, len(table))
	for _, m := range table {
		out = append(out, m.Name)
	}
	return out
}

// Gauge is one gauge family of the table. Every value of its label
// vocabulary reads 0 until a recording package binds a source, so the
// series exists before the package that fills it is built: the registry
// drops a gauge collector that returns nothing.
type Gauge struct {
	labels []label

	mu   sync.Mutex
	srcs map[string]func() float64
}

// Bind gives an unlabelled gauge its source, read at every scrape.
func (g *Gauge) Bind(fn func() float64) { g.BindFor("", fn) }

// BindFor gives one label value of a gauge its source. The value must be
// in the vocabulary of the table, and a gauge with more than one label
// has none: no metric of the table needs that.
func (g *Gauge) BindFor(value string, fn func() float64) {
	if !slices.Contains(g.values(), value) {
		panic(fmt.Sprintf("metrics: %q is not a value of this gauge", value))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.srcs[value] = fn
}

// values reports the label values this gauge reports one series for. An
// unlabelled gauge has the single empty value.
func (g *Gauge) values() []string {
	if len(g.labels) == 0 {
		return []string{""}
	}
	return g.labels[0].Values
}

// collect is the registry's scrape-time callback.
func (g *Gauge) collect() []pkgmetrics.LabeledValue {
	g.mu.Lock()
	srcs := maps.Clone(g.srcs)
	g.mu.Unlock()
	out := make([]pkgmetrics.LabeledValue, 0, len(srcs))
	for _, v := range g.values() {
		var labels map[string]string
		if v != "" {
			labels = map[string]string{g.labels[0].Name: v}
		}
		var value float64
		if fn := srcs[v]; fn != nil {
			value = fn()
		}
		out = append(out, pkgmetrics.LabeledValue{Labels: labels, Value: value})
	}
	return out
}

// Set is every handle of the table, one field per metric, handed to the
// packages that record them. A field is never nil: a metric of a spec
// that is not built yet has a handle nothing writes through.
type Set struct {
	// The write-ahead log (spec 004).
	WALCommits         *pkgmetrics.Counter
	WALCommitConflicts *pkgmetrics.Counter
	WALCommitRetries   *pkgmetrics.Counter
	WALEntryBytes      *pkgmetrics.Counter
	WALHeadCheck       *pkgmetrics.Histogram

	// Smart HTTP (spec 003).
	Pushes         *pkgmetrics.Counter
	PushesRejected *pkgmetrics.Counter
	Fetches        *pkgmetrics.Counter
	PushDuration   *pkgmetrics.Histogram

	// The repository cache (spec 004).
	RepoMaterialized   *pkgmetrics.Counter
	RepoEntriesApplied *pkgmetrics.Counter
	RepoRebuilt        *pkgmetrics.Counter
	RepoMaterialize    *pkgmetrics.Histogram

	// The public listener (spec 011).
	Requests         *pkgmetrics.Counter
	RequestDuration  *pkgmetrics.Histogram
	RequestsInFlight *Gauge

	// Placement and the cache ceiling (spec 005).
	CacheBytes    *Gauge
	CacheRepos    *Gauge
	Evictions     *pkgmetrics.Counter
	GossipPackets *pkgmetrics.Counter

	// Compaction (spec 006).
	Compactions       *pkgmetrics.Counter
	CompactionSeconds *pkgmetrics.Histogram

	// The authorizer client (spec 007).
	AuthorizerSeconds *pkgmetrics.Histogram

	// Event delivery (spec 008).
	EventsDelivered *pkgmetrics.Counter
	EventsDead      *pkgmetrics.Counter

	// Limits (spec 012).
	RateLimited *pkgmetrics.Counter

	// The store adapter and the breakers (spec 015).
	StorageOps         *pkgmetrics.Counter
	StorageSeconds     *pkgmetrics.Histogram
	StorageBreaker     *Gauge
	StaleResponses     *pkgmetrics.Counter
	LogIntegrityErrors *pkgmetrics.Counter

	// The weekly sweep (spec 019).
	OrphanObjects *Gauge
	StorageBytes  *Gauge

	// The SSH listener (spec 024), whose three series that spec owns in
	// a table of its own.
	SSHSessions *pkgmetrics.Counter
	SSHAuth     *pkgmetrics.Counter
	SSHKeys     *pkgmetrics.Histogram
}

// Register registers every metric of the table on reg and returns the
// handles. A nil registry gets one of its own, which is what a test of a
// recording package that asserts on nothing wants.
func Register(reg *pkgmetrics.Registry) *Set {
	if reg == nil {
		reg = pkgmetrics.NewRegistry()
	}
	r := registrar{
		counters:   map[string]*pkgmetrics.Counter{},
		histograms: map[string]*pkgmetrics.Histogram{},
		gauges:     map[string]*Gauge{},
	}
	for _, m := range table {
		switch m.Kind {
		case counter:
			c := reg.Counter(m.Name, m.Help)
			// The series reads 0 before the first event, so a dashboard
			// and an alert see the metric from the first scrape.
			for _, labels := range combinations(m.Labels) {
				c.Add(labels, 0)
			}
			r.counters[m.Name] = c
		case histogram:
			r.histograms[m.Name] = reg.Histogram(m.Name, m.Help, m.Buckets)
		case gauge:
			g := &Gauge{labels: m.Labels, srcs: map[string]func() float64{}}
			reg.Gauge(m.Name, m.Help, g.collect)
			r.gauges[m.Name] = g
		}
	}
	return &Set{
		WALCommits:         r.counter("origo_wal_commits_total"),
		WALCommitConflicts: r.counter("origo_wal_commit_conflicts_total"),
		WALCommitRetries:   r.counter("origo_wal_commit_retries_total"),
		WALEntryBytes:      r.counter("origo_wal_entry_bytes_total"),
		WALHeadCheck:       r.histogram("origo_wal_head_check_seconds"),

		Pushes:         r.counter("origo_pushes_total"),
		PushesRejected: r.counter("origo_pushes_rejected_total"),
		Fetches:        r.counter("origo_fetches_total"),
		PushDuration:   r.histogram("origo_push_duration_seconds"),

		RepoMaterialized:   r.counter("origo_repo_materialized_total"),
		RepoEntriesApplied: r.counter("origo_repo_entries_applied_total"),
		RepoRebuilt:        r.counter("origo_repo_rebuilt_total"),
		RepoMaterialize:    r.histogram("origo_repo_materialize_seconds"),

		Requests:         r.counter("origo_requests_total"),
		RequestDuration:  r.histogram("origo_request_duration_seconds"),
		RequestsInFlight: r.gauge("origo_requests_in_flight"),

		CacheBytes:    r.gauge("origo_cache_bytes"),
		CacheRepos:    r.gauge("origo_cache_repos"),
		Evictions:     r.counter("origo_evictions_total"),
		GossipPackets: r.counter("origo_gossip_packets_total"),

		Compactions:       r.counter("origo_compactions_total"),
		CompactionSeconds: r.histogram("origo_compaction_seconds"),

		AuthorizerSeconds: r.histogram("origo_authorizer_seconds"),

		EventsDelivered: r.counter("origo_events_delivered_total"),
		EventsDead:      r.counter("origo_events_dead_total"),

		RateLimited: r.counter("origo_rate_limited_total"),

		StorageOps:         r.counter("origo_storage_ops_total"),
		StorageSeconds:     r.histogram("origo_storage_seconds"),
		StorageBreaker:     r.gauge("origo_storage_breaker_state"),
		StaleResponses:     r.counter("origo_stale_responses_total"),
		LogIntegrityErrors: r.counter("origo_log_integrity_errors_total"),

		OrphanObjects: r.gauge("origo_orphan_objects"),
		StorageBytes:  r.gauge("origo_storage_bytes"),

		SSHSessions: r.counter("origo_ssh_sessions_total"),
		SSHAuth:     r.counter("origo_ssh_auth_total"),
		SSHKeys:     r.histogram("origo_ssh_keys_seconds"),
	}
}

// registrar holds what the walk over the table built, keyed by name, so
// a field of Set and its row cannot drift: a name the table does not
// define with that type panics at start-up, before a scrape can miss it.
type registrar struct {
	counters   map[string]*pkgmetrics.Counter
	histograms map[string]*pkgmetrics.Histogram
	gauges     map[string]*Gauge
}

func (r registrar) counter(name string) *pkgmetrics.Counter {
	c, ok := r.counters[name]
	if !ok {
		panic("metrics: no counter " + name + " in the table")
	}
	return c
}

func (r registrar) histogram(name string) *pkgmetrics.Histogram {
	h, ok := r.histograms[name]
	if !ok {
		panic("metrics: no histogram " + name + " in the table")
	}
	return h
}

func (r registrar) gauge(name string) *Gauge {
	g, ok := r.gauges[name]
	if !ok {
		panic("metrics: no gauge " + name + " in the table")
	}
	return g
}

// combinations reports every label set a metric's vocabularies allow: one
// nil set for a metric with no label, and nothing at all for a metric
// with an open vocabulary, whose series appear as they are recorded.
func combinations(labels []label) []map[string]string {
	out := []map[string]string{nil}
	for _, l := range labels {
		if len(l.Values) == 0 {
			return nil
		}
		next := make([]map[string]string, 0, len(out)*len(l.Values))
		for _, base := range out {
			for _, v := range l.Values {
				m := map[string]string{l.Name: v}
				maps.Copy(m, base)
				next = append(next, m)
			}
		}
		out = next
	}
	return out
}
