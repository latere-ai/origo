// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package repo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // the packfile trailer git defines
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/tracing"
	"github.com/latere-ai/origo/internal/wal"
)

// Errors the cache reports.
var (
	// ErrNotFound means the log has no such repository.
	ErrNotFound = errors.New("repo: not found")
	// ErrDeleted means the newest index marks the repository deleted.
	ErrDeleted = errors.New("repo: deleted")
	// ErrCorrupt means the local copy failed verification and was
	// removed; the next open rebuilds it.
	ErrCorrupt = errors.New("repo: local copy corrupt")
)

// Options configures a Cache.
type Options struct {
	// Dir is the data directory, ORIGO_DATA_DIR.
	Dir string
	Log *wal.Log
	// GitBin is the git binary, "git" by default.
	GitBin string
	// GitTimeout bounds one git invocation, 5 minutes by default.
	GitTimeout time.Duration
	// FsckEvery runs a sampled connectivity check on every Nth open of a
	// repository; 0 disables it. 256 by default.
	FsckEvery int
	// Workers is how many entries Apply fetches and indexes at once
	// (spec 005, materialization budget). DefaultWorkers when zero.
	Workers int
	// Now is the clock the last-acquired times of the copies run on,
	// which the evictor of spec 005 ranks by, and the last-check times
	// the stale bound of spec 015 runs on. The wall clock by default.
	Now func() time.Time
	// StaleMax is ORIGO_STALE_MAX (spec 015): how long a warm copy is
	// served without a currency check while the read breaker is open,
	// measured from the last check that answered. DefaultStaleMax when
	// zero.
	StaleMax time.Duration
	// DropCapability is ORIGO_TEST_DROP_CAPABILITY (spec 021): the one
	// capability of spec 003's table whose repository configuration
	// key is written off instead of on, so the node stops advertising
	// it. Empty in every deployment; the mutation job sets it.
	DropCapability string
	Logger         *slog.Logger
	Metrics        *metrics.Set
}

// capabilityKeys is the repository configuration key behind each
// git-controlled capability of spec 003's table. allow-tip-sha1-in-want
// and allow-reachable-sha1-in-want share one key, so dropping either
// drops both.
var capabilityKeys = map[string]string{
	"filter":                       "uploadpack.allowFilter",
	"allow-tip-sha1-in-want":       "uploadpack.allowAnySHA1InWant",
	"allow-reachable-sha1-in-want": "uploadpack.allowAnySHA1InWant",
	"atomic":                       "receive.advertiseAtomic",
	"push-options":                 "receive.advertisePushOptions",
}

// DefaultStaleMax is ORIGO_STALE_MAX's default.
const DefaultStaleMax = 5 * time.Minute

// DefaultWorkers is the number of concurrent entry workers of Apply.
const DefaultWorkers = 4

// Cache holds the materialized repositories of one node.
type Cache struct {
	dir       string
	log       *wal.Log
	git       *Git
	fsckEvery int
	workers   int
	now       func() time.Time
	staleMax  time.Duration
	// dropCapability is Options.DropCapability.
	dropCapability string
	logger         *slog.Logger
	// maxBatchEntries and maxBatchBytes are the batch bounds of
	// applyEntries, the constants below; a test lowers them.
	maxBatchEntries int
	maxBatchBytes   int64
	// batchSplits counts the joined packs indexed one pack at a time,
	// for the test of that path.
	batchSplits atomic.Int64

	mu    sync.Mutex
	repos map[string]*Repo

	materialized *pkgmetrics.Counter
	applied      *pkgmetrics.Counter
	rebuilt      *pkgmetrics.Counter
	materialize  *pkgmetrics.Histogram
	stale        *pkgmetrics.Counter
	integrity    *pkgmetrics.Counter
}

// Repo is one materialized repository. Seq and Index describe the state
// the local copy holds; they are read and changed only under the lock.
type Repo struct {
	ID  string
	Dir string
	// Seq is the sequence applied last; Local says whether a local copy
	// exists at all.
	Seq   uint64
	Local bool
	// Index is the index object applied last, the base of a commit.
	Index *wal.Index

	lock  sync.RWMutex
	opens int

	// held mirrors Seq and Local for a reader that holds no lock: the
	// sequence plus one, 0 when there is no local copy. Gossip (spec
	// 005) reads it to decide whether an announcement needs a catch-up.
	held atomic.Uint64
	// bytes is the size of the directory, measured after every apply
	// and once at start-up; acquired is the time of the last Acquire,
	// as UnixNano. The evictor of spec 005 ranks copies by both.
	bytes    atomic.Int64
	acquired atomic.Int64
	// checked is the time of the last currency check that answered,
	// as UnixNano, written by Acquire on every check that answers and
	// read by Lease when the read breaker is open to decide whether the
	// copy may be served stale (spec 015). An evicted copy has none.
	checked atomic.Int64
}

// Lease is one acquired repository: the copy, and whether it was served
// without a currency check because the read breaker is open (spec 015),
// with the time since the last check that answered, which the handler
// sends as Origo-Stale in whole seconds.
type Lease struct {
	*Repo
	Stale    bool
	StaleFor time.Duration
	release  func()
}

// Release unlocks the repository.
func (l *Lease) Release() { l.release() }

// Copy describes one local copy for the evictor of spec 005.
type Copy struct {
	ID string
	// Bytes is the size of the copy's directory.
	Bytes int64
	// Acquired is the time of the last Acquire, or the process start
	// for a copy found on disk that no request has acquired yet.
	Acquired time.Time
}

// Git exposes the subprocess wrapper for the smart HTTP handlers.
func (c *Cache) Git() *Git { return c.git }

// Log exposes the log.
func (c *Cache) Log() *wal.Log { return c.log }

// New prepares the cache under the data directory.
func New(o Options) (*Cache, error) {
	if o.Dir == "" || o.Log == nil {
		return nil, errors.New("repo: Dir and Log are required")
	}
	bin := o.GitBin
	if bin == "" {
		bin = "git"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("repo: git binary: %w", err)
	}
	home := filepath.Join(o.Dir, "home")
	for _, d := range []string{filepath.Join(o.Dir, "repos"), filepath.Join(o.Dir, "spool"), home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	timeout := o.GitTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	set := o.Metrics
	if set == nil {
		set = metrics.Register(nil)
	}
	fsckEvery := o.FsckEvery
	if fsckEvery == 0 {
		fsckEvery = 256
	}
	workers := o.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	staleMax := o.StaleMax
	if staleMax <= 0 {
		staleMax = DefaultStaleMax
	}
	c := &Cache{
		dir: o.Dir, log: o.Log, git: &Git{Bin: path, Home: home, Timeout: timeout},
		fsckEvery: fsckEvery, workers: workers, now: now, staleMax: staleMax, dropCapability: o.DropCapability, logger: logger, repos: map[string]*Repo{},
		maxBatchEntries: MaxBatchEntries, maxBatchBytes: MaxBatchBytes,
		materialized: set.RepoMaterialized, applied: set.RepoEntriesApplied,
		rebuilt: set.RepoRebuilt, materialize: set.RepoMaterialize,
		stale: set.StaleResponses, integrity: set.LogIntegrityErrors,
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// load walks repos/ once at start-up and registers every copy on disk
// with its size, acquired at the process start, so a restarted node
// keeps its warm set for the evictor's floor and ranks it by use from
// then on (spec 005).
func (c *Cache) load() error {
	entries, err := os.ReadDir(filepath.Join(c.dir, "repos"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".origo.json")
		if !ok || !wal.ValidID(id) {
			continue
		}
		r := c.get(id)
		if r.Local {
			r.bytes.Store(dirBytes(r.Dir))
		}
	}
	return nil
}

// dirBytes is the size of every regular file under dir.
func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil //nolint:nilerr // an entry that cannot be read is not counted, and the walk goes on
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// measure records the copy's size after an apply.
func (c *Cache) measure(r *Repo) { r.bytes.Store(dirBytes(r.Dir)) }

// setHeld mirrors Seq and Local into the lock-free field.
func (r *Repo) setHeld() {
	if r.Local {
		r.held.Store(r.Seq + 1)
	} else {
		r.held.Store(0)
	}
}

// Held reports the sequence the local copy of id holds, without
// waiting on its lock: an announcement for a repository the node does
// not hold is dropped (spec 005). A copy being written reports the
// sequence it held before the write.
func (c *Cache) Held(id string) (seq uint64, ok bool) {
	c.mu.Lock()
	r := c.repos[id]
	c.mu.Unlock()
	if r == nil {
		return 0, false
	}
	h := r.held.Load()
	if h == 0 {
		return 0, false
	}
	return h - 1, true
}

// Copies lists every local copy with its size and last acquire, for
// the evictor of spec 005.
func (c *Cache) Copies() []Copy {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Copy, 0, len(c.repos))
	for _, r := range c.repos {
		if r.held.Load() == 0 {
			continue
		}
		out = append(out, Copy{ID: r.ID, Bytes: r.bytes.Load(), Acquired: time.Unix(0, r.acquired.Load())})
	}
	return out
}

// Stats reports the bytes and the number of the local copies, the two
// gauges of spec 005.
func (c *Cache) Stats() (bytes int64, repos int) {
	for _, cp := range c.Copies() {
		bytes += cp.Bytes
		repos++
	}
	return bytes, repos
}

// TryEvict removes the local copy of id when no request holds it and
// reports whether it did: the evictor takes the write lock, so it
// never removes a copy mid-request or mid-apply (spec 005), and it
// skips a copy in use rather than wait behind the request.
func (c *Cache) TryEvict(id string) bool {
	c.mu.Lock()
	r := c.repos[id]
	c.mu.Unlock()
	if r == nil || !r.lock.TryLock() {
		return false
	}
	defer r.lock.Unlock()
	if !r.Local {
		return false
	}
	c.evict(r)
	return true
}

// SpoolDir is where request bodies larger than memory are spooled.
func (c *Cache) SpoolDir() string { return filepath.Join(c.dir, "spool") }

// Dir is the data directory.
func (c *Cache) Dir() string { return c.dir }

// repoDir is the bare repository of id.
func (c *Cache) repoDir(id string) string { return filepath.Join(c.dir, "repos", id+".git") }

// stateFile is origo.json beside the repository, which records the
// sequence the local copy holds.
func (c *Cache) stateFile(id string) string { return filepath.Join(c.dir, "repos", id+".origo.json") }

type state struct {
	Seq uint64 `json:"seq"`
}

// get returns the in-memory record of id, reading its state from disk on
// first use.
func (c *Cache) get(id string) *Repo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.repos[id]; ok {
		return r
	}
	r := &Repo{ID: id, Dir: c.repoDir(id)}
	if data, err := os.ReadFile(c.stateFile(id)); err == nil {
		var st state
		if json.Unmarshal(data, &st) == nil {
			if _, err := os.Stat(filepath.Join(r.Dir, "HEAD")); err == nil {
				r.Seq, r.Local = st.Seq, true
			}
		}
	}
	r.setHeld()
	r.acquired.Store(c.now().UnixNano())
	c.repos[id] = r
	return r
}

// Acquire locks the repository for reading or writing and brings the
// local copy current with the log before returning it. The release
// function must be called when the caller is done. ErrNotFound and
// ErrDeleted are the two refusals a handler turns into a 404. A reader
// under an open read breaker may get a stale copy without knowing;
// Lease is Acquire for a caller that must know.
func (c *Cache) Acquire(ctx context.Context, id string, write bool) (*Repo, func(), error) {
	l, err := c.Lease(ctx, id, write)
	if err != nil {
		return nil, nil, err
	}
	return l.Repo, l.Release, nil
}

// Lease is Acquire with the stale verdict of spec 015. When the read
// breaker refuses the currency check, a reader of a warm copy whose
// last check that answered is within StaleMax gets the copy marked
// Stale; a cold copy, a copy past the bound, and every writer get the
// refusal, wal.ErrStorageOpen, which the handlers map to 503
// storage_unavailable: a write needs a current index object as its
// base, so it is refused before it does any work.
func (c *Cache) Lease(ctx context.Context, id string, write bool) (*Lease, error) {
	if !wal.ValidID(id) {
		return nil, ErrNotFound
	}
	r := c.get(id)
	r.acquired.Store(c.now().UnixNano())
	if write {
		r.lock.Lock()
	} else {
		r.lock.RLock()
	}
	release := func() {
		if write {
			r.lock.Unlock()
		} else {
			r.lock.RUnlock()
		}
	}
	newest, err := c.sync(ctx, r, write)
	if errors.Is(err, errNeedsWrite) {
		// A reader found the copy behind: upgrade, catch up, downgrade.
		// The index the check read is applied as it is when the copy
		// did not move while the reader waited for the write lock,
		// which is the common case, so a catch-up costs one check;
		// otherwise the check runs again under the lock.
		seq, local := r.Seq, r.Local
		r.lock.RUnlock()
		r.lock.Lock()
		if newest != nil && r.Seq == seq && r.Local == local {
			err = c.applyNewest(ctx, r, newest)
		} else {
			_, err = c.sync(ctx, r, true)
		}
		r.lock.Unlock()
		r.lock.RLock()
	}
	if err != nil {
		if lease, ok := c.staleLease(r, write, err, release); ok {
			return lease, nil
		}
		release()
		return nil, err
	}
	return &Lease{Repo: r, release: release}, nil
}

// staleLease decides whether a refused check may be served from the
// copy: the refusal is the read breaker's, the caller reads, the copy
// is warm with an index and a last check that answered, and that check
// is within StaleMax. The caller holds the lock.
func (c *Cache) staleLease(r *Repo, write bool, err error, release func()) (*Lease, bool) {
	if write || !errors.Is(err, wal.ErrStorageOpen) || !r.Local || r.Index == nil || r.Index.DeletedAt != nil {
		return nil, false
	}
	checked := r.checked.Load()
	if checked == 0 {
		return nil, false
	}
	age := c.now().Sub(time.Unix(0, checked))
	if age > c.staleMax {
		return nil, false
	}
	c.stale.Inc(nil)
	return &Lease{Repo: r, Stale: true, StaleFor: age, release: release}, true
}

var errNeedsWrite = errors.New("repo: catch-up needs the write lock")

// applyNewest brings the copy to the newest index under the write lock,
// or evicts it when the index marks the repository deleted.
func (c *Cache) applyNewest(ctx context.Context, r *Repo, newest *wal.Index) error {
	if newest.DeletedAt != nil {
		c.evict(r)
		r.Index = newest
		return ErrDeleted
	}
	return c.Apply(ctx, r, newest)
}

// newest is the currency check under the index.check span of spec 011's
// read path: one HEAD on the index object after the one the copy holds,
// and the read of a newer index when the HEAD finds one.
func (c *Cache) newest(ctx context.Context, r *Repo, held uint64, local bool) (*wal.Index, bool, error) {
	ctx, end := tracing.Start(ctx, "index.check", tracing.Repo(r.ID))
	defer end()
	return c.log.Newest(ctx, r.ID, held, local)
}

// sync runs the currency check and applies what the local copy lacks.
// Without the write lock it only reports whether work is needed, with
// the newer index it read so the caller applies it under the lock.
func (c *Cache) sync(ctx context.Context, r *Repo, write bool) (*wal.Index, error) {
	if r.Index != nil && r.Local && write && c.fsckEvery > 0 {
		r.opens++
		if r.opens%c.fsckEvery == 0 {
			if err := c.Verify(ctx, r); err != nil {
				return nil, err
			}
		}
	}
	newest, changed, err := c.newest(ctx, r, r.Seq, r.Local)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// The check answered: the copy may be served stale from here for
	// StaleMax once the breaker opens (spec 015).
	r.checked.Store(c.now().UnixNano())
	if !changed && r.Index == nil {
		// A local copy from a previous process: its index object is
		// read once so a commit has a base.
		if !write {
			return nil, errNeedsWrite
		}
		ix, err := c.log.ReadIndex(ctx, r.ID, r.Seq)
		switch {
		case err == nil:
			r.Index = ix
		case errors.Is(err, wal.ErrNotFound):
			// The sequence the copy holds is no longer in the log: the
			// bucket was reset under a warm cache, or the object was
			// swept. HEAD index/<n+1> answered 404 for the wrong reason,
			// so the copy proves nothing; it is removed and the
			// repository is materialized from whatever the log holds
			// now, which is ErrNotFound when it holds nothing.
			c.logger.WarnContext(ctx, "held sequence is not in the log, rebuilding the copy", "repo", r.ID, "seq", r.Seq)
			c.evict(r)
			newest, changed, err = c.newest(ctx, r, 0, false)
			if err != nil {
				if errors.Is(err, wal.ErrNotFound) {
					return nil, ErrNotFound
				}
				return nil, err
			}
		default:
			return nil, err
		}
	}
	if !changed {
		if r.Index.DeletedAt != nil {
			return nil, ErrDeleted
		}
		return nil, nil
	}
	if !write {
		return newest, errNeedsWrite
	}
	return nil, c.applyNewest(ctx, r, newest)
}

// Apply brings the local copy to ix: packs first, then every entry above
// the local sequence, then the reference map reconciled to the index so
// the result equals the index whatever the copy held before. The caller
// holds the write lock; a push handler calls it with the index another
// writer committed while the push was in flight.
func (c *Cache) Apply(ctx context.Context, r *Repo, ix *wal.Index) error {
	ctx, end := tracing.Start(ctx, "materialize", tracing.Repo(r.ID))
	defer end()
	start := time.Now()
	fresh := !r.Local
	if fresh {
		if err := c.initBare(ctx, r); err != nil {
			return err
		}
		c.materialized.Inc(nil)
	}
	// Every listed pack missing under objects/pack is fetched whatever
	// the copy holds: a copy that lost a pack file, or one that followed
	// a compaction whose compacted_through its sequence already passed,
	// would otherwise be served from an incomplete object store. A pack
	// on disk costs one stat.
	for _, p := range ix.Packs {
		if err := c.fetchPack(ctx, r, p); err != nil {
			return c.recordIntegrity(ctx, r, err)
		}
	}
	var pending []wal.IndexEntry
	for _, e := range ix.Entries {
		if e.Seq > r.Seq {
			pending = append(pending, e)
		}
	}
	if err := c.applyEntries(ctx, r, pending); err != nil {
		if isIntegrity(err) {
			return c.recordIntegrity(ctx, r, err)
		}
		if IsCorruption(err) {
			c.rebuilt.Inc(nil)
			c.evict(r)
			return fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		if IsMissingBase(err) {
			// A thin pack whose base is in no entry and no pack, with
			// the copy itself sound: an integrity error of the log like
			// a missing pack, counted as one, and answered
			// storage_unavailable (spec 015), because git's refusal
			// names no key and the base may still arrive.
			c.integrity.Inc(nil)
			c.logger.ErrorContext(ctx, "log integrity error: a thin pack's base is in no entry", "repo", r.ID, "error", err)
			return &wal.OpError{Op: "index-pack", Err: err}
		}
		return err
	}
	if err := c.reconcileRefs(ctx, r, ix); err != nil {
		return err
	}
	if err := c.writeState(r, ix.Seq); err != nil {
		return err
	}
	r.Seq, r.Local, r.Index = ix.Seq, true, ix
	r.setHeld()
	c.measure(r)
	c.materialize.Observe(nil, time.Since(start).Seconds())
	return nil
}

// isIntegrity reports whether err is a wal.IntegrityError.
func isIntegrity(err error) bool {
	var ie *wal.IntegrityError
	return errors.As(err, &ie)
}

// recordIntegrity counts and logs an integrity error of the log (spec
// 015) and returns it unchanged: the key is in the line for the operator
// who restores the object, nothing is evicted, and the next request
// materializes again. An error that is not one passes through.
func (c *Cache) recordIntegrity(ctx context.Context, r *Repo, err error) error {
	var ie *wal.IntegrityError
	if !errors.As(err, &ie) {
		return err
	}
	c.integrity.Inc(nil)
	c.logger.ErrorContext(ctx, wal.IntegrityMessage(ie.Err), "repo", r.ID, "key", ie.Key, "error", ie.Err)
	return err
}

func (c *Cache) initBare(ctx context.Context, r *Repo) error {
	if err := os.RemoveAll(r.Dir); err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	if _, err := c.git.Run(ctx, r.Dir, nil, "init", "-q", "--bare", "."); err != nil {
		return err
	}
	// Objects from a client are checked on receive and on every other
	// transfer, a tree entry that would name the git directory on an
	// NTFS or HFS+ file system is refused (spec 016), the local copy is
	// never garbage collected on its own (compaction is a log entry,
	// spec 006), and the capabilities spec 003 promises are advertised,
	// except the one the mutation job of spec 021 drops.
	dropped := capabilityKeys[c.dropCapability]
	for _, kv := range [][2]string{
		{"core.protectNTFS", "true"},
		{"core.protectHFS", "true"},
		{"receive.fsckObjects", "true"},
		{"transfer.fsckObjects", "true"},
		{"receive.advertiseAtomic", "true"},
		{"receive.advertisePushOptions", "true"},
		{"receive.autogc", "false"},
		{"gc.auto", "0"},
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.allowAnySHA1InWant", "true"},
	} {
		if kv[0] == dropped {
			kv[1] = "false"
		}
		if _, err := c.git.Run(ctx, r.Dir, nil, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// fetchPack downloads one pack the index lists and its .idx into
// objects/pack under the name git reads, wal.PackFile of the key. The
// .idx lands first so git never sees a pack without one. A pack already
// on disk under that name is not fetched again, so a current copy pays
// one stat per listed pack.
func (c *Cache) fetchPack(ctx context.Context, r *Repo, key string) error {
	dir := filepath.Join(r.Dir, "objects", "pack")
	if _, err := os.Stat(filepath.Join(dir, wal.PackFile(key))); err == nil {
		return nil
	}
	for _, k := range []string{strings.TrimSuffix(key, ".pack") + ".idx", key} {
		full := c.log.RepoPrefix(r.ID) + k
		rc, _, err := c.log.Store().Get(ctx, full, "")
		if errors.Is(err, wal.ErrNotFound) {
			// A pack the index names is gone: corruption in the log,
			// never an outage (spec 015).
			return &wal.IntegrityError{Key: full, Err: err}
		}
		if err != nil {
			return fmt.Errorf("repo: pack %s: %w", k, err)
		}
		err = writeAtomic(filepath.Join(dir, wal.PackFile(k)), rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Batch bounds of applyEntries: consecutive entries whose packs are
// joined into one pack for one index-pack run.
const (
	// MaxBatchEntries is the most entries one index-pack run takes.
	MaxBatchEntries = 256
	// MaxBatchBytes is the most pack bytes one index-pack run takes.
	MaxBatchBytes = 256 << 20
)

// spooled is one fetched entry: its pack on disk, verified against
// the entry's length and digest, or the error that stopped it.
type spooled struct {
	path  string
	size  int64
	count uint32
	err   error
}

// applyEntries brings the entries into the object store (spec 005,
// materialization budget). Workers, 4 by default, each take the next
// entry in sequence order, download it, verify its length and
// pack_sha256, and spool the pack. One indexer consumes the spooled
// packs in sequence order, joining consecutive ones into a single pack
// for one index-pack --fix-thin --strict run, at most MaxBatchEntries
// or MaxBatchBytes per run: a pack from one push is thin against the
// pushes before it, so indexing in order means every base is in the
// batch or already in the store, and one subprocess lands many
// entries. The reference transactions are not applied per entry;
// reconcileRefs sets the whole map once. The first error stops every
// worker and the indexer.
func (c *Cache) applyEntries(ctx context.Context, r *Repo, entries []wal.IndexEntry) error {
	if len(entries) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		cond  = sync.NewCond(&mu)
		ready = make([]*spooled, len(entries))
	)
	defer func() {
		for _, sp := range ready {
			if sp != nil && sp.path != "" {
				_ = os.Remove(sp.path)
			}
		}
	}()
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(c.workers, len(entries)) {
		wg.Go(func() {
			for i := range next {
				sp := c.spoolEntry(ctx, r, entries[i])
				mu.Lock()
				ready[i] = sp
				mu.Unlock()
				cond.Broadcast()
				if sp.err != nil {
					cancel()
					return
				}
			}
		})
	}
	go func() {
		defer close(next)
		for i := range entries {
			select {
			case next <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	// The indexer: wait until the batch window from position i is
	// spooled, or a worker failed, then index the window as one pack.
	err := func() error {
		for i := 0; i < len(entries); {
			end := min(i+c.maxBatchEntries, len(entries))
			mu.Lock()
			for ctx.Err() == nil && !spooledThrough(ready, i, end) {
				cond.Wait()
			}
			var batch []*spooled
			var bytes int64
			j := i
			for ; j < end && ready[j] != nil && ready[j].err == nil; j++ {
				if ready[j].path == "" {
					continue
				}
				if len(batch) > 0 && bytes+ready[j].size > c.maxBatchBytes {
					break
				}
				batch = append(batch, ready[j])
				bytes += ready[j].size
			}
			if j == i {
				err := ctx.Err()
				if ready[i] != nil && ready[i].err != nil {
					err = ready[i].err
				}
				mu.Unlock()
				return err
			}
			mu.Unlock()
			if len(batch) > 0 {
				if err := c.indexBatch(ctx, r, batch); err != nil {
					return err
				}
			}
			for _, sp := range batch {
				_ = os.Remove(sp.path)
				sp.path = ""
			}
			c.applied.Add(nil, uint64(j-i)) //nolint:gosec // a count is never negative
			i = j
		}
		return nil
	}()
	cancel()
	cond.Broadcast()
	wg.Wait()
	// A worker's failure cancels the context, and the indexer may have
	// been running git on an earlier batch, or another worker fetching
	// an earlier entry, at that moment; the worker's error is the cause
	// and is what the caller sees, not a cancelled run or a cancelled
	// fetch. A context the caller ended leaves only cancellations, and
	// the first is reported.
	if err != nil {
		var first error
		for _, sp := range ready {
			if sp == nil || sp.err == nil {
				continue
			}
			if !errors.Is(sp.err, context.Canceled) {
				return sp.err
			}
			if first == nil {
				first = sp.err
			}
		}
		if first != nil {
			return first
		}
	}
	return err
}

// spooledThrough reports whether every position in [from, to) is spooled.
func spooledThrough(ready []*spooled, from, to int) bool {
	for _, sp := range ready[from:to] {
		if sp == nil {
			return false
		}
	}
	return true
}

// spoolEntry fetches one entry, checks its header against the index
// row, and spools its pack with its length and digest verified. An
// entry with no pack spools nothing.
func (c *Cache) spoolEntry(ctx context.Context, r *Repo, e wal.IndexEntry) *spooled {
	full := c.log.RepoPrefix(r.ID) + e.Key
	// An entry the index names that is gone, does not parse, or carries
	// another sequence, length, or digest than its row is corruption in
	// the log (spec 015): an IntegrityError naming the key.
	integrity := func(err error) *spooled { return &spooled{err: &wal.IntegrityError{Key: full, Err: err}} }
	rc, _, err := c.log.Store().Get(ctx, full, "")
	if errors.Is(err, wal.ErrNotFound) {
		return integrity(err)
	}
	if err != nil {
		return &spooled{err: fmt.Errorf("repo: entry %s: %w", e.Key, err)}
	}
	defer func() { _ = rc.Close() }()
	h, _, pack, err := wal.ReadEntryHead(rc)
	if err != nil {
		return integrity(err)
	}
	if h.Seq != e.Seq || h.Kind != e.Kind {
		return integrity(fmt.Errorf("repo: entry carries seq %d kind %s, the index says %d %s", h.Seq, h.Kind, e.Seq, e.Kind))
	}
	if h.PackBytes == 0 {
		return &spooled{}
	}
	f, err := os.CreateTemp(c.SpoolDir(), "entry-*.pack")
	if err != nil {
		return &spooled{err: err}
	}
	sp := &spooled{path: f.Name(), size: h.PackBytes}
	fail := func(err error) *spooled {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return &spooled{err: err}
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(pack, h.PackBytes+1))
	if err != nil {
		return fail(err)
	}
	if n != h.PackBytes || hex.EncodeToString(sum.Sum(nil)) != h.PackSHA256 {
		return fail(&wal.IntegrityError{Key: full, Err: fmt.Errorf("repo: pack of %d bytes with digest %s does not match the entry (%d bytes, %s)", n, hex.EncodeToString(sum.Sum(nil)), h.PackBytes, h.PackSHA256)})
	}
	var head [packHeaderSize]byte
	if _, err := f.ReadAt(head[:], 0); err != nil || string(head[:4]) != "PACK" || binary.BigEndian.Uint32(head[4:8]) != 2 || n < packHeaderSize+packTrailerSize {
		return fail(&wal.IntegrityError{Key: full, Err: errors.New("repo: the pack is not a version 2 packfile")})
	}
	sp.count = binary.BigEndian.Uint32(head[8:12])
	if err := f.Close(); err != nil {
		return fail(err)
	}
	return sp
}

// The packfile framing: a 12 byte header (PACK, version 2, the object
// count) and a 20 byte SHA-1 trailer over everything before it. The
// objects between are self-delimiting, an OFS_DELTA offset is relative
// to its own object, and a REF_DELTA base is named by hash, so the
// bodies of consecutive packs joined under one header and one trailer
// are one pack, which index-pack reads like any other.
const (
	packHeaderSize  = 12
	packTrailerSize = 20
)

// indexBatch joins the spooled packs into one pack in the spool and
// indexes it into the object store. Git puts no object twice in one
// pack, but two entries can carry the same object (two clients pushing
// one blob, a pack built against a stale advertisement), and a joined
// pack with a duplicate is refused by index-pack --strict; a batch of
// several packs that fails is then indexed one pack at a time in
// sequence order, each with --fix-thin, which is what one run per
// entry would have done, and a pack that still fails is the error.
func (c *Cache) indexBatch(ctx context.Context, r *Repo, batch []*spooled) error {
	err := c.indexJoined(ctx, r, batch)
	if err == nil || len(batch) == 1 {
		return err
	}
	c.logger.WarnContext(ctx, "joined pack refused, indexing its packs one by one", "repo", r.ID, "packs", len(batch), "error", err)
	c.batchSplits.Add(1)
	for _, sp := range batch {
		f, err := os.Open(sp.path)
		if err != nil {
			return err
		}
		err = c.indexPackFile(ctx, r, f)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// indexJoined indexes the spooled packs as one pack.
func (c *Cache) indexJoined(ctx context.Context, r *Repo, batch []*spooled) error {
	joined, err := os.CreateTemp(c.SpoolDir(), "batch-*.pack")
	if err != nil {
		return err
	}
	defer func() { _ = joined.Close(); _ = os.Remove(joined.Name()) }()
	var count uint32
	for _, sp := range batch {
		count += sp.count
	}
	var head [packHeaderSize]byte
	copy(head[:4], "PACK")
	binary.BigEndian.PutUint32(head[4:8], 2)
	binary.BigEndian.PutUint32(head[8:12], count)
	sum := sha1.New() //nolint:gosec // the pack trailer git defines
	out := io.MultiWriter(joined, sum)
	if _, err := out.Write(head[:]); err != nil {
		return err
	}
	for _, sp := range batch {
		f, err := os.Open(sp.path)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.NewSectionReader(f, packHeaderSize, sp.size-packHeaderSize-packTrailerSize))
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	if _, err := joined.Write(sum.Sum(nil)); err != nil {
		return err
	}
	return c.indexPackFile(ctx, r, joined)
}

// indexPackFile runs index-pack over a spooled pack and moves the
// result into objects/pack. Git writes the completed pack, its index,
// and its reverse index beside a path in the spool, and the three are
// renamed into objects/pack under the content name git printed, index
// first, so no reader sees a pack without its index, and a failed run
// leaves nothing but a file in the spool, which is removed.
func (c *Cache) indexPackFile(ctx context.Context, r *Repo, f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.CreateTemp(c.SpoolDir(), "index-*.pack")
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(out.Name(), ".pack")
	_ = out.Close()
	_ = os.Remove(out.Name()) // git refuses to create an existing path
	defer func() {
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			_ = os.Remove(base + ext)
		}
	}()
	stdout, err := c.git.Run(ctx, r.Dir, f, "index-pack", "--stdin", "--fix-thin", "--strict", base+".pack")
	if err != nil {
		return err
	}
	_, name, ok := strings.Cut(strings.TrimSpace(string(stdout)), "\t")
	if !ok || name == "" {
		return fmt.Errorf("repo: index-pack printed %q", stdout)
	}
	dir := filepath.Join(r.Dir, "objects", "pack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, ext := range []string{".idx", ".rev", ".pack"} {
		if err := os.Rename(base+ext, filepath.Join(dir, "pack-"+name+ext)); err != nil && (ext != ".rev" || !errors.Is(err, os.ErrNotExist)) {
			return err
		}
	}
	return nil
}

// reconcileRefs makes the local references exactly the index's map.
func (c *Cache) reconcileRefs(ctx context.Context, r *Repo, ix *wal.Index) error {
	out, err := c.git.Run(ctx, r.Dir, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return err
	}
	local := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if name, sha, ok := strings.Cut(sc.Text(), " "); ok {
			local[name] = sha
		}
	}
	var script bytes.Buffer
	for name, sha := range ix.Refs {
		if name == "HEAD" {
			continue
		}
		if local[name] != sha {
			fmt.Fprintf(&script, "update %s %s\n", name, sha)
		}
	}
	for name := range local {
		if _, ok := ix.Refs[name]; !ok {
			fmt.Fprintf(&script, "delete %s\n", name)
		}
	}
	if script.Len() > 0 {
		if _, err := c.git.Run(ctx, r.Dir, &script, "update-ref", "--stdin"); err != nil {
			return err
		}
	}
	if head, ok := ix.Refs["HEAD"]; ok {
		cur, _ := c.git.Run(ctx, r.Dir, nil, "symbolic-ref", "HEAD")
		if want := strings.TrimPrefix(head, "ref: "); strings.TrimSpace(string(cur)) != want {
			if _, err := c.git.Run(ctx, r.Dir, nil, "symbolic-ref", "HEAD", want); err != nil {
				return err
			}
		}
	}
	return nil
}

// Advance records that the local copy now holds ix, after the caller
// applied it through git's own path (a push this node served).
func (c *Cache) Advance(r *Repo, ix *wal.Index) error {
	if err := c.writeState(r, ix.Seq); err != nil {
		return err
	}
	r.Seq, r.Local, r.Index = ix.Seq, true, ix
	r.setHeld()
	c.measure(r)
	return nil
}

func (c *Cache) writeState(r *Repo, seq uint64) error {
	data, err := json.Marshal(state{Seq: seq})
	if err != nil {
		return err
	}
	return writeAtomic(c.stateFile(r.ID), bytes.NewReader(data))
}

// Verify runs a connectivity check and evicts the copy when it fails, so
// the next open rebuilds it from the log.
func (c *Cache) Verify(ctx context.Context, r *Repo) error {
	if _, err := c.git.Run(ctx, r.Dir, nil, "fsck", "--connectivity-only", "--no-progress"); err != nil {
		c.rebuilt.Inc(nil)
		c.evict(r)
		c.logger.WarnContext(ctx, "local copy corrupt, evicted for rebuild", "repo", r.ID, "error", err)
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return nil
}

// Evict removes the local copy of id. The next open materializes it.
func (c *Cache) Evict(id string) {
	r := c.get(id)
	r.lock.Lock()
	defer r.lock.Unlock()
	c.evict(r)
}

func (c *Cache) evict(r *Repo) {
	_ = os.RemoveAll(r.Dir)
	_ = os.Remove(c.stateFile(r.ID))
	r.Seq, r.Local, r.Index = 0, false, nil
	r.setHeld()
	r.bytes.Store(0)
	r.checked.Store(0)
}

func writeAtomic(path string, src io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
