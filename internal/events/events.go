// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package events is the push event channel of spec 008: one signed HTTP
// request per acknowledged push, delivered at least once, and the one
// channel every other event kind of the deck goes through. Enqueue
// writes the event object of a committed push entry, Emit the object of
// an event without a sequence, and the Dispatcher delivers them, retries
// on the fixed schedule, dead-letters after the window, keeps the
// per-repository cursor, and repairs what a dead node left behind from
// its journals and the index objects.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/wal"
)

// Namespace is the UUID v5 namespace of every event id, fixed by spec
// 008 so the enqueue and the repair sweep produce one id for one entry.
const Namespace = "7c1f0b6e-4d0a-4b6a-9d3e-2a8f5c1e9b47"

// The headers of a delivery.
const (
	HeaderSignature = "Origo-Signature"
	HeaderEvent     = "Origo-Event"
	HeaderDelivery  = "Origo-Delivery"
)

// KindPush is the kind of the event this package builds itself; every
// other kind is given to Emit by its owner.
const KindPush = "push"

// The values of kind_detail: what a push that changes no branch or tag
// did instead. DetailUndelete is never on the wire, because an undelete
// has exactly one event, spec 019's undeleted; the value exists so the
// enqueue and the repair sweep both recognise the entry and skip it.
const (
	DetailDefaultBranch = "default_branch"
	DetailUndelete      = "undelete"
)

// FailpointBeforeEnqueue is spec 002's events.before-enqueue: reached
// after the index object is created and the verdict is delivered,
// before the event object is written.
const FailpointBeforeEnqueue = "events.before-enqueue"

// The fixed durations of the Design.
const (
	// DeliveryTimeout bounds one POST to the sink.
	DeliveryTimeout = 10 * time.Second
	// Window is how long after its at an event is retried before it
	// moves to the dead-letter prefix.
	Window = 24 * time.Hour
	// FlushInterval is how often the in-memory journal is written.
	FlushInterval = 10 * time.Second
	// RepairLag is how old a pending object's next_at, or an entry's at,
	// must be before a sweep touches it: what a live node is about to
	// handle itself is left alone.
	RepairLag = time.Minute
	// JournalKeep is how long a node's journal objects are kept.
	JournalKeep = 2 * 24 * time.Hour
)

// Delays is the retry schedule: the delay before the next attempt after
// attempt n, the last entry repeating.
var Delays = []time.Duration{time.Second, 10 * time.Second, time.Minute, 10 * time.Minute, time.Hour}

// Delay is the wait after the given number of failed attempts.
func Delay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(Delays) {
		attempts = len(Delays)
	}
	return Delays[attempts-1]
}

var namespace = uuid.MustParse(Namespace)

// PushID is the id of the push event of one entry: the UUID v5 of
// <repo>:<seq>, the sequence as the 12 digit zero-padded decimal of the
// log key.
func PushID(repo string, seq uint64) string {
	return uuid.NewSHA1(namespace, []byte(repo+":"+wal.SeqString(seq))).String()
}

// EmitID is the id of an event without a sequence: the UUID v5 of
// <repo>:<kind>:<at>, at in RFC 3339 with second precision in UTC, so a
// repeated Emit of one operation is one event.
func EmitID(repo, kind string, at time.Time) string {
	return uuid.NewSHA1(namespace, []byte(repo+":"+kind+":"+at.UTC().Truncate(time.Second).Format(time.RFC3339))).String()
}

// Update is one reference update of a push event.
type Update struct {
	Ref    string `json:"ref"`
	Before string `json:"before"`
	After  string `json:"after"`
	Forced bool   `json:"forced"`
}

// Pusher is the identity of a push or an operation: the effective
// subject and, when a service acted on its behalf, the actor.
type Pusher struct {
	Sub   string `json:"sub"`
	Actor string `json:"actor"`
}

// Push is the payload of a push event, spec 003's with kind and the two
// optional fields of spec 008.
type Push struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Repo       string    `json:"repo"`
	Seq        uint64    `json:"seq"`
	Owner      string    `json:"owner"`
	Slug       string    `json:"slug"`
	Pusher     Pusher    `json:"pusher"`
	Updates    []Update  `json:"updates"`
	At         time.Time `json:"at"`
	KindDetail string    `json:"kind_detail,omitempty"`
	Operation  string    `json:"operation,omitempty"`
}

// Entry is a committed push entry as Enqueue and the repair sweep see
// it: the header and the transaction, and for the receive path the
// references whose update was not a fast-forward, computed while the
// pushed objects were still in quarantine. The sweep has no objects to
// compute it from and reports forced false.
type Entry struct {
	Header wal.Header
	Refs   []wal.RefUpdate
	Forced map[string]bool
}

// Detail is the kind_detail of a transaction: DetailUndelete for an
// empty one, DetailDefaultBranch for one that moves HEAD alone, and
// empty for a push that changes a branch or a tag.
func Detail(refs []wal.RefUpdate) string {
	if len(refs) == 0 {
		return DetailUndelete
	}
	for _, u := range refs {
		if u.Ref != "HEAD" {
			return ""
		}
	}
	return DetailDefaultBranch
}

// The push options the payload reads.
const (
	optionOff       = "origo.event=off"
	optionOperation = "origo.operation="
)

// build is the payload of an entry, or false when the entry produces no
// event: an undelete, or a push whose options carry origo.event=off.
func build(repo string, m *wal.Meta, e Entry) (Push, bool) {
	if slices.Contains(e.Header.PushOptions, optionOff) {
		return Push{}, false
	}
	detail := Detail(e.Refs)
	if detail == DetailUndelete {
		return Push{}, false
	}
	p := Push{
		ID: PushID(repo, e.Header.Seq), Kind: KindPush, Repo: repo, Seq: e.Header.Seq,
		Owner: m.Owner, Slug: m.Slug, Pusher: Pusher{Sub: e.Header.Subject, Actor: e.Header.Actor},
		At: e.Header.At.UTC(), KindDetail: detail, Updates: make([]Update, 0, len(e.Refs)),
	}
	for _, u := range e.Refs {
		p.Updates = append(p.Updates, Update{Ref: u.Ref, Before: u.Old, After: u.New, Forced: e.Forced[u.Ref]})
	}
	for _, o := range e.Header.PushOptions {
		if name, ok := strings.CutPrefix(o, optionOperation); ok {
			p.Operation = name
		}
	}
	return p, true
}

// object is one event object: the payload as delivered, the attempts so
// far, and when the next one is due.
type object struct {
	Event    json.RawMessage `json:"event"`
	Attempts int             `json:"attempts"`
	NextAt   time.Time       `json:"next_at"`
}

// envelope is what the dispatcher reads back from a payload of any kind.
type envelope struct {
	ID   string    `json:"id"`
	Kind string    `json:"kind"`
	Repo string    `json:"repo"`
	Seq  uint64    `json:"seq"`
	At   time.Time `json:"at"`
}

// Membership is the live set of spec 005 as the repair sweep reads it:
// when each node was last heard, by heartbeat or by an announcement
// that carried a valid MAC. A nil Membership reports every other node
// as never heard, which is the single-node case and the state before
// spec 005 lands.
type Membership interface {
	// LastHeard reports when node was last heard and false when it was
	// never heard since this node started.
	LastHeard(node string) (time.Time, bool)
}

// Options configures a Dispatcher.
type Options struct {
	// Log is the write-ahead log the events sit beside: its store, its
	// prefix, the repository metadata, and the index objects the repair
	// sweep reads. Required.
	Log *wal.Log
	// Node is ORIGO_NODE_NAME, the journal's owner. Required.
	Node string
	// URL and Secret are ORIGO_EVENTS_URL and ORIGO_EVENTS_SECRET. An
	// empty URL turns events off: every method is a no-op.
	URL    string
	Secret string
	// Client posts the deliveries. A client with DeliveryTimeout by
	// default; a given client keeps its own timeout.
	Client *http.Client
	// Now is the clock. The wall clock by default.
	Now func() time.Time
	// Members is the live set; nil reports every other node unheard.
	Members Membership
	// RepairInterval and RepairUnheard are ORIGO_REPAIR_INTERVAL and
	// ORIGO_REPAIR_UNHEARD; 10 and 5 minutes by default.
	RepairInterval time.Duration
	RepairUnheard  time.Duration
	Metrics        *metrics.Registry
	Logger         *slog.Logger
	// Failpoint, when set, is called at FailpointBeforeEnqueue and aborts
	// the enqueue with its error. Nil in every deployment.
	Failpoint func(name string) error
}

// Dispatcher owns the event objects of one node: what it enqueued or
// emitted, what the sweep handed it, and its journal.
type Dispatcher struct {
	log     *wal.Log
	store   wal.Store
	prefix  string
	node    string
	url     string
	secret  string
	client  *http.Client
	now     func() time.Time
	members Membership
	logger  *slog.Logger
	fail    func(string) error

	repairInterval time.Duration
	repairUnheard  time.Duration

	delivered *metrics.Counter
	dead      *metrics.Counter

	// queue is every pending key this node delivers, with its due time,
	// and wake tells the loop the queue changed.
	mu    sync.Mutex
	queue map[string]time.Time
	wake  chan struct{}

	// cursorMu serialises the read-modify-write of one node's cursors.
	cursorMu sync.Mutex

	journal *journal
}

// New builds a dispatcher. Nothing runs until Run.
func New(o Options) (*Dispatcher, error) {
	if o.Log == nil || o.Node == "" {
		return nil, errors.New("events: Log and Node are required")
	}
	if o.URL != "" && o.Secret == "" {
		return nil, errors.New("events: a URL needs a secret")
	}
	d := &Dispatcher{
		log: o.Log, store: o.Log.Store(), prefix: o.Log.Prefix() + "events/", node: o.Node,
		url: o.URL, secret: o.Secret, client: o.Client, now: o.Now, members: o.Members, logger: o.Logger, fail: o.Failpoint,
		repairInterval: o.RepairInterval, repairUnheard: o.RepairUnheard,
		queue: map[string]time.Time{}, wake: make(chan struct{}, 1),
	}
	if d.client == nil {
		// Pooled connections with a header deadline and no proxy from
		// the environment on a path that carries a signature, the
		// shape of cmd/origod's outbound transport.
		d.client = &http.Client{Timeout: DeliveryTimeout, Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: DeliveryTimeout, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: DeliveryTimeout,
			TLSHandshakeTimeout:   5 * time.Second,
		}}
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	if d.fail == nil {
		d.fail = func(string) error { return nil }
	}
	if d.repairInterval <= 0 {
		d.repairInterval = 10 * time.Minute
	}
	if d.repairUnheard <= 0 {
		d.repairUnheard = 5 * time.Minute
	}
	reg := o.Metrics
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	d.delivered = reg.Counter("origo_events_delivered_total", "events answered 2xx by the sink")
	d.dead = reg.Counter("origo_events_dead_total", "events moved to the dead-letter prefix after the window")
	d.delivered.Add(nil, 0)
	d.dead.Add(nil, 0)
	d.journal = newJournal()
	return d, nil
}

// Enabled reports whether a sink is configured. A nil dispatcher is
// off, so a caller passes one through without a check.
func (d *Dispatcher) Enabled() bool { return d != nil && d.url != "" }

// Keys under the events prefix.
func (d *Dispatcher) pushKey(repo string, seq uint64) string {
	return d.prefix + repo + "/" + wal.SeqString(seq) + ".json"
}
func (d *Dispatcher) emitKey(repo, id string) string { return d.prefix + repo + "/a-" + id + ".json" }
func (d *Dispatcher) cursorKey(repo string) string   { return d.prefix + repo + "/cursor" }
func (d *Dispatcher) deadKey(key string) string {
	return d.prefix + "dead/" + strings.TrimPrefix(key, d.prefix)
}
func (d *Dispatcher) journalPrefix(node string) string { return d.prefix + "nodes/" + node + "/" }
func (d *Dispatcher) journalKey(node string, day time.Time) string {
	return d.journalPrefix(node) + day.UTC().Format(time.DateOnly) + ".log"
}

// isEmitKey reports whether key holds an event without a sequence.
func isEmitKey(key string) bool { return strings.HasPrefix(key[strings.LastIndex(key, "/")+1:], "a-") }

// Enqueue writes the event object of a committed push entry, after the
// journal line, and hands it to the delivery loop. It writes nothing
// for an undelete, for a push whose options carry origo.event=off, or
// when events are off. A failed write is logged and returned; the
// caller acknowledges the push all the same, because the repair sweep
// rebuilds the event from the index.
func (d *Dispatcher) Enqueue(ctx context.Context, repo string, e Entry) error {
	if !d.Enabled() {
		return nil
	}
	if err := d.fail(FailpointBeforeEnqueue); err != nil {
		return err
	}
	now := d.now()
	if first := d.journal.add(now, repo, e.Header.Seq); first {
		if err := d.flushJournal(ctx); err != nil {
			d.logger.WarnContext(ctx, "event journal not flushed", "node", d.node, "error", err)
		}
	}
	m, err := d.log.ReadMeta(ctx, repo)
	if err != nil {
		return d.enqueueFailed(ctx, repo, e.Header.Seq, err)
	}
	p, ok := build(repo, m, e)
	if !ok {
		return nil
	}
	key := d.pushKey(repo, e.Header.Seq)
	if err := d.write(ctx, key, p, now, false); err != nil {
		return d.enqueueFailed(ctx, repo, e.Header.Seq, err)
	}
	d.schedule(key, now)
	return nil
}

func (d *Dispatcher) enqueueFailed(ctx context.Context, repo string, seq uint64, err error) error {
	d.logger.ErrorContext(ctx, "event not enqueued; the repair sweep writes it from the index", "repo", repo, "seq", seq, "error", err)
	return fmt.Errorf("events: enqueue %s %d: %w", repo, seq, err)
}

// Emit writes the object of an event without a sequence: kind with the
// shared fields (id, kind, repo, owner, slug, at, pusher) around extra,
// under a-<id>.json, so a repeated Emit of one operation with one at
// writes one key. It writes no journal line, because there is no entry
// to rebuild the event from, and does nothing when events are off. at
// is kept to the second in UTC, the precision the id is derived from.
func (d *Dispatcher) Emit(ctx context.Context, repo, kind string, at time.Time, pusher Pusher, extra map[string]any) error {
	if !d.Enabled() {
		return nil
	}
	if kind == "" || kind == KindPush {
		return fmt.Errorf("events: emit needs a kind other than %q", kind)
	}
	m, err := d.log.ReadMeta(ctx, repo)
	if err != nil {
		return d.emitFailed(ctx, repo, kind, err)
	}
	at = at.UTC().Truncate(time.Second)
	id := EmitID(repo, kind, at)
	payload := maps.Clone(extra)
	if payload == nil {
		payload = map[string]any{}
	}
	payload["id"], payload["kind"], payload["repo"] = id, kind, repo
	payload["owner"], payload["slug"], payload["at"], payload["pusher"] = m.Owner, m.Slug, at, pusher
	key := d.emitKey(repo, id)
	if err := d.write(ctx, key, payload, d.now(), false); err != nil {
		return d.emitFailed(ctx, repo, kind, err)
	}
	d.schedule(key, d.now())
	return nil
}

func (d *Dispatcher) emitFailed(ctx context.Context, repo, kind string, err error) error {
	d.logger.ErrorContext(ctx, "event not emitted", "repo", repo, "kind", kind, "error", err)
	return fmt.Errorf("events: emit %s %s: %w", kind, repo, err)
}

// write stores a fresh object with attempts 0 and next_at now, by
// create-if-absent when ifAbsent is set, which is how two sweeps
// repairing one entry produce one object.
func (d *Dispatcher) write(ctx context.Context, key string, payload any, now time.Time, ifAbsent bool) error {
	event, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data, err := json.Marshal(object{Event: event, NextAt: now.UTC()})
	if err != nil {
		return err
	}
	if ifAbsent {
		_, err = d.store.Create(ctx, key, wal.BytesBody(data))
	} else {
		_, err = d.store.Put(ctx, key, wal.BytesBody(data))
	}
	return err
}

// schedule queues key for delivery at due and wakes the loop. A key
// already queued keeps the earlier of the two times.
func (d *Dispatcher) schedule(key string, due time.Time) {
	d.mu.Lock()
	if cur, ok := d.queue[key]; !ok || due.Before(cur) {
		d.queue[key] = due
	}
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Pending reports the keys queued on this node, for the tests.
func (d *Dispatcher) Pending() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Sorted(maps.Keys(d.queue))
}

// Run runs the delivery loop, the journal flush, and the repair sweep
// until ctx ends, after the start-up repair over the node's own
// journals. It returns at once when events are off.
func (d *Dispatcher) Run(ctx context.Context) error {
	if !d.Enabled() {
		<-ctx.Done()
		return ctx.Err()
	}
	d.startup(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { d.runDeliver(ctx) })
	wg.Go(func() { d.runJournal(ctx) })
	wg.Go(func() { d.runRepair(ctx) })
	wg.Wait()
	// The lines held in memory outlive the cancelled context on purpose.
	stopCtx := context.WithoutCancel(ctx)
	if err := d.flushJournal(stopCtx); err != nil {
		d.logger.WarnContext(stopCtx, "event journal not flushed at stop", "node", d.node, "error", err)
	}
	return ctx.Err()
}

// startup loads the node's own journals of today and yesterday, so a
// restart under the same name keeps the lines an earlier process
// wrote, and runs the repair step over them once.
func (d *Dispatcher) startup(ctx context.Context) {
	now := d.now()
	for _, day := range []time.Time{now, now.Add(-24 * time.Hour)} {
		lines, err := d.readJournal(ctx, d.node, day)
		if err != nil {
			d.logger.WarnContext(ctx, "own event journal not read", "node", d.node, "error", err)
			continue
		}
		d.journal.load(day, lines)
	}
	d.repairNode(ctx, d.node, now)
}
