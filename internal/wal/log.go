// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/retry"
	"latere.ai/x/pkg/wait"
)

// Options configures a Log.
type Options struct {
	Store Store
	// Prefix is the key prefix of everything the log writes, "origo/" by
	// default.
	Prefix string
	// Now stamps entries. The wall clock by default.
	Now func() time.Time
	// Metrics receives the log's series. A fresh registry by default.
	Metrics *metrics.Registry
	// Failpoint, when set, is called with a name at each injectable point
	// and aborts the operation with its error. Nil in every deployment.
	Failpoint func(name string) error
	// Logger receives the warnings the log does not turn into errors.
	Logger *slog.Logger
	// MaxCommitAttempts bounds the rounds one commit plays against other
	// writers before it gives up. 4096 by default: every lost round is a
	// sequence another writer committed, so the bound is never met by a
	// live repository, and a caller's context ends a commit sooner.
	MaxCommitAttempts int
	// Sleep waits between lost rounds. The wall clock by default; a test
	// substitutes it.
	Sleep func(context.Context, time.Duration)
}

// Log is the write-ahead log of every repository under one prefix.
type Log struct {
	store       Store
	prefix      string
	now         func() time.Time
	failpoint   func(string) error
	logger      *slog.Logger
	maxAttempts int
	sleep       func(context.Context, time.Duration)

	commits   *metrics.Counter
	conflicts *metrics.Counter
	retries   *metrics.Counter
	entryByte *metrics.Counter
	headCheck *metrics.Histogram
}

// Failpoint names. The end-to-end suite kills a node at them.
const (
	FailpointBeforeIndex = "commit.before-index"
)

// New returns a log over the store.
func New(o Options) *Log {
	l := &Log{
		store: o.Store, prefix: o.Prefix, now: o.Now, failpoint: o.Failpoint,
		logger: o.Logger, maxAttempts: o.MaxCommitAttempts, sleep: o.Sleep,
	}
	if l.sleep == nil {
		l.sleep = func(ctx context.Context, d time.Duration) { _ = wait.Sleep(ctx, d) }
	}
	if l.prefix == "" {
		l.prefix = "origo/"
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.failpoint == nil {
		l.failpoint = func(string) error { return nil }
	}
	if l.logger == nil {
		l.logger = slog.Default()
	}
	if l.maxAttempts <= 0 {
		l.maxAttempts = 4096
	}
	reg := o.Metrics
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	l.commits = reg.Counter("origo_wal_commits_total", "index objects this node created")
	l.conflicts = reg.Counter("origo_wal_commit_conflicts_total", "commits refused because a reference moved")
	l.retries = reg.Counter("origo_wal_commit_retries_total", "commit rounds lost to another writer and replayed")
	l.entryByte = reg.Counter("origo_wal_entry_bytes_total", "bytes written as entries")
	l.headCheck = reg.Histogram("origo_wal_head_check_seconds", "latency of the HEAD currency check",
		[]float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5})
	// A scrape before the first event reads the series at 0 rather than
	// not at all, which is the shape spec 011's dashboards expect.
	for _, c := range []*metrics.Counter{l.commits, l.conflicts, l.retries, l.entryByte} {
		c.Add(nil, 0)
	}
	return l
}

// Store exposes the store, for the readiness check and the tests.
func (l *Log) Store() Store { return l.store }

// RepoPrefix is the key prefix of one repository.
func (l *Log) RepoPrefix(repo string) string { return l.prefix + "repos/" + repo + "/" }

func (l *Log) key(repo, rel string) string { return l.RepoPrefix(repo) + rel }

// Ping proves the bucket answers: one listing of at most one key.
func (l *Log) Ping(ctx context.Context) error {
	_, err := l.store.List(ctx, ListOptions{Prefix: l.prefix, Max: 1})
	return err
}

// ReadIndex fetches and validates index/<seq>.
func (l *Log) ReadIndex(ctx context.Context, repo string, seq uint64) (*Index, error) {
	rc, _, err := l.store.Get(ctx, l.key(repo, IndexKey(seq)), "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxIndexBytes+1))
	if err != nil {
		return nil, err
	}
	ix, err := ParseIndex(data)
	if err != nil {
		return nil, err
	}
	if ix.Seq != seq {
		return nil, fmt.Errorf("wal: %s carries seq %d", IndexKey(seq), ix.Seq)
	}
	return ix, nil
}

// HasIndex is the currency check: HEAD index/<seq>, one round trip.
func (l *Log) HasIndex(ctx context.Context, repo string, seq uint64) (bool, error) {
	start := time.Now()
	_, err := l.store.Head(ctx, l.key(repo, IndexKey(seq)))
	l.headCheck.Observe(nil, time.Since(start).Seconds())
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Hint reads index/latest. It may lag and never leads correctness.
func (l *Log) Hint(ctx context.Context, repo string) (uint64, error) {
	rc, _, err := l.store.Get(ctx, l.key(repo, LatestKey), "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()
	var hint struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.NewDecoder(io.LimitReader(rc, 1024)).Decode(&hint); err != nil {
		return 0, fmt.Errorf("wal: hint: %w", err)
	}
	return hint.Seq, nil
}

func (l *Log) writeHint(ctx context.Context, repo string, seq uint64) {
	body := BytesBody([]byte(`{"seq":` + strconv.FormatUint(seq, 10) + `}`))
	if _, err := l.store.Put(ctx, l.key(repo, LatestKey), body); err != nil {
		l.logger.WarnContext(ctx, "index/latest hint not written", "repo", repo, "seq", seq, "error", err)
	}
}

// highestIndex lists index/ and returns the highest sequence, or false
// when the repository has no index object at all.
func (l *Log) highestIndex(ctx context.Context, repo string) (uint64, bool, error) {
	prefix := l.key(repo, "index/0")
	var (
		best  uint64
		found bool
		after string
	)
	for {
		res, err := l.store.List(ctx, ListOptions{Prefix: prefix, StartAfter: after, Max: 1000})
		if err != nil {
			return 0, false, err
		}
		for _, o := range res.Objects {
			if seq, ok := ParseIndexKey(o.Key[len(l.RepoPrefix(repo)):]); ok {
				best, found = seq, true
			}
			after = o.Key
		}
		if !res.Truncated || len(res.Objects) == 0 {
			return best, found, nil
		}
	}
}

// Newest finds the newest index object. held is the sequence the caller
// has, or false in haveHeld when it has nothing. It answers nil with
// changed false when the caller is current, which costs one HEAD that
// answers 404; otherwise it returns the newest index object, which lists
// every entry the caller lacks. ErrNotFound means the repository has no
// index at all.
func (l *Log) Newest(ctx context.Context, repo string, held uint64, haveHeld bool) (ix *Index, changed bool, err error) {
	var m uint64
	if haveHeld {
		ok, err := l.HasIndex(ctx, repo, held+1)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		m = held + 1
		if h, err := l.Hint(ctx, repo); err == nil && h > m {
			if ok, err := l.HasIndex(ctx, repo, h); err == nil && ok {
				m = h
			}
		}
	} else {
		// The hint may lag, be missing, or be garbage; it never leads
		// correctness, so any failure to use it falls back to a listing.
		h, err := l.Hint(ctx, repo)
		if err != nil && !errors.Is(err, ErrNotFound) {
			l.logger.WarnContext(ctx, "index/latest hint unusable", "repo", repo, "error", err)
		}
		ok := false
		if err == nil {
			if ok, err = l.HasIndex(ctx, repo, h); err != nil {
				return nil, false, err
			}
		}
		if ok {
			m = h
		} else {
			best, found, err := l.highestIndex(ctx, repo)
			if err != nil {
				return nil, false, err
			}
			if !found {
				return nil, false, ErrNotFound
			}
			m = best
		}
	}
	for {
		ok, err := l.HasIndex(ctx, repo, m+1)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			break
		}
		m++
	}
	ix, err = l.ReadIndex(ctx, repo, m)
	if err != nil {
		return nil, false, err
	}
	return ix, true, nil
}

// Entry is what a writer commits.
type Entry struct {
	Kind    Kind
	Subject string
	Actor   string
	// Refs is the reference transaction. Old must equal the current value
	// of every reference named, or the commit refuses with a
	// ConflictError; that is git's own rule for a push.
	Refs []RefUpdate
	// Pack is the packfile, absent when Size is zero.
	Pack Body
	// Packs replaces the index's pack list; compaction sets it. Nil keeps
	// the list.
	Packs []string
	// PacksBytes is the size of the .pack objects Packs lists, which the
	// writer of a compact entry knows because it uploaded them. A compact
	// commit sets the index's size_bytes to it; every other kind ignores
	// it.
	PacksBytes int64
	// CompactedThrough is set by compaction; zero keeps the value.
	CompactedThrough uint64
	// Deleted marks the repository deleted (KindDelete) or, when false on
	// a KindPush entry, clears an earlier deletion.
	Deleted bool
	// PushOptions are recorded in the header for spec 008.
	PushOptions []string
}

// ConflictError reports a reference that moved under a writer.
type ConflictError struct {
	Ref, Expected, Actual string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("wal: %s is at %s, expected %s", e.Ref, short(e.Actual), short(e.Expected))
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ErrContended is returned when a commit lost more rounds than allowed.
var ErrContended = errors.New("wal: commit lost too many rounds to other writers")

// Committed is the outcome of a commit.
type Committed struct {
	Index *Index
	// Key is the entry's key relative to the repository prefix.
	Key string
	// Header is the entry's header as written, what spec 008's event is
	// built from without a second read of the entry.
	Header Header
	// EntryDuration and IndexDuration are the time spent writing the
	// entry and creating the index object, summed over lost rounds; the
	// receive path observes them as the entry and index phases of
	// origo_push_duration_seconds (spec 011).
	EntryDuration, IndexDuration time.Duration
}

// Commit writes the entry and commits it by creating the next index
// object. The caller holds base, the index it applied last. When another
// writer commits first, catchUp is called with the newer index so the
// caller applies it locally, the transaction is checked against it, and
// the round is replayed at the next sequence. A reference that moved
// refuses the whole commit with a ConflictError.
func (l *Log) Commit(ctx context.Context, repo string, base *Index, e Entry, catchUp func(context.Context, *Index) error) (*Committed, error) {
	var entryTime, indexTime time.Duration
	for attempt := range l.maxAttempts {
		if attempt > 0 {
			l.retries.Inc(nil)
			// Writers that lost the same round would otherwise replay it
			// in step and lose again together; a jittered pause spreads
			// them out so every one of them lands.
			l.sleep(ctx, commitBackoff(attempt))
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		if err := l.checkTransaction(base, e.Refs); err != nil {
			l.conflicts.Inc(nil)
			return nil, err
		}
		seq := base.Seq + 1
		if seq > maxSeq {
			return nil, errors.New("wal: sequence space exhausted")
		}
		started := time.Now()
		key, hdr, err := l.writeEntry(ctx, repo, seq, e)
		entryTime += time.Since(started)
		if err != nil {
			return nil, err
		}
		at := hdr.At
		if err := l.failpoint(FailpointBeforeIndex); err != nil {
			return nil, err
		}
		next, err := l.nextIndex(base, seq, key, at, e)
		if err != nil {
			return nil, err
		}
		data, err := EncodeIndex(next)
		if err != nil {
			return nil, err
		}
		started = time.Now()
		_, err = l.store.Create(ctx, l.key(repo, IndexKey(seq)), BytesBody(data))
		indexTime += time.Since(started)
		won := err == nil
		if err != nil {
			// A 412 means another writer created it, unless it is our own
			// create whose response was lost. Any other failure may also
			// have been applied, so the answer is read back either way.
			stored, rerr := l.ReadIndex(ctx, repo, seq)
			switch {
			case rerr == nil && stored.Entry == key:
				won = true
			case rerr == nil:
				if err := catchUp(ctx, stored); err != nil {
					return nil, err
				}
				base = stored
				continue
			case errors.Is(err, ErrExists):
				return nil, fmt.Errorf("wal: %s exists but cannot be read: %w", IndexKey(seq), rerr)
			default:
				return nil, err
			}
		}
		if won {
			l.commits.Inc(nil)
			l.writeHint(ctx, repo, seq)
			return &Committed{Index: next, Key: key, Header: hdr, EntryDuration: entryTime, IndexDuration: indexTime}, nil
		}
	}
	return nil, ErrContended
}

// commitPolicy is the pause before replaying a lost round: up to 1ms
// doubled per consecutive loss, capped at 16ms, fully jittered. It stays
// small because a loser already pays a read and a re-upload before its
// next attempt; the jitter only breaks lockstep between losers.
var commitPolicy = retry.Policy{Base: time.Millisecond, Max: 16 * time.Millisecond, Jitter: 1}

func commitBackoff(lost int) time.Duration {
	return commitPolicy.Delay(lost)
}

func (l *Log) checkTransaction(base *Index, refs []RefUpdate) error {
	for _, u := range refs {
		cur, ok := base.Refs[u.Ref]
		if !ok {
			cur = ZeroSHA
		}
		if cur != u.Old {
			return &ConflictError{Ref: u.Ref, Expected: u.Old, Actual: cur}
		}
	}
	return nil
}

// writeEntry uploads header, transaction, and pack as one object under a
// fresh nonce and returns the key relative to the repository prefix and
// the header as written, whose at the index object records for a push.
func (l *Log) writeEntry(ctx context.Context, repo string, seq uint64, e Entry) (string, Header, error) {
	var nonce [8]byte
	if _, err := cryptorand.Read(nonce[:]); err != nil {
		return "", Header{}, err
	}
	h := Header{
		V: Version, Kind: e.Kind, Seq: seq, At: l.now().UTC(), Subject: e.Subject, Actor: e.Actor,
		PackBytes: e.Pack.Size, PackSHA256: e.Pack.SHA256, PushOptions: e.PushOptions,
	}
	if e.Pack.Size == 0 {
		h.PackSHA256 = ""
	}
	head, err := EncodeEntryHead(h, e.Refs)
	if err != nil {
		return "", Header{}, err
	}
	body, err := concatBody(head, e.Pack)
	if err != nil {
		return "", Header{}, err
	}
	key := EntryKey(seq, hex.EncodeToString(nonce[:]))
	if _, err := l.store.Put(ctx, l.key(repo, key), body); err != nil {
		return "", Header{}, fmt.Errorf("wal: write entry: %w", err)
	}
	l.entryByte.Add(nil, uint64(body.Size)) //nolint:gosec // a size is never negative
	return key, h, nil
}

func (l *Log) nextIndex(base *Index, seq uint64, key string, at time.Time, e Entry) (*Index, error) {
	next := base.Clone()
	next.V = Version
	next.Seq = seq
	next.Entry = key
	for _, u := range e.Refs {
		if u.New == ZeroSHA {
			delete(next.Refs, u.Ref)
		} else {
			next.Refs[u.Ref] = u.New
		}
	}
	ie := IndexEntry{Seq: seq, Key: key, Kind: e.Kind, PackSHA256: e.Pack.SHA256}
	if e.Pack.Size == 0 {
		ie.PackSHA256 = ""
	}
	if e.Kind == KindCompact {
		next.Packs = e.Packs
		next.CompactedThrough = e.CompactedThrough
		next.Entries = nil
	}
	next.Entries = append(next.Entries, ie)
	// size_bytes is what the log holds: the listed packs plus the pack
	// bytes of the entries since the last compaction. A compaction
	// resets it to its packs, so the figure falls to what a fresh
	// materialization downloads; a push adds its own pack, a delete and
	// an empty push add 0.
	if e.Kind == KindCompact {
		next.SizeBytes = e.PacksBytes
	} else {
		next.SizeBytes += e.Pack.Size
	}
	if e.Kind == KindPush {
		// The newest push's at; every other kind copies it forward
		// through Clone.
		next.PushedAt = &at
	}
	switch {
	case e.Kind == KindDelete:
		next.DeletedAt = &at
	case !e.Deleted:
		next.DeletedAt = nil
	}
	if _, err := ParseIndex(mustJSON(next)); err != nil {
		return nil, err
	}
	return next, nil
}

func mustJSON(ix *Index) []byte {
	b, _ := EncodeIndex(ix)
	return b
}
