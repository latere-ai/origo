// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package repo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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

	"latere.ai/x/pkg/metrics"

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
	// which the evictor of spec 005 ranks by. The wall clock by default.
	Now     func() time.Time
	Logger  *slog.Logger
	Metrics *metrics.Registry
}

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
	logger    *slog.Logger

	mu    sync.Mutex
	repos map[string]*Repo

	// thinRetries counts index-pack runs repeated after every lower
	// entry landed, for the test of the thin-pack retry.
	thinRetries atomic.Int64

	materialized *metrics.Counter
	applied      *metrics.Counter
	rebuilt      *metrics.Counter
	materialize  *metrics.Histogram
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
}

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
	reg := o.Metrics
	if reg == nil {
		reg = metrics.NewRegistry()
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
	c := &Cache{
		dir: o.Dir, log: o.Log, git: &Git{Bin: path, Home: home, Timeout: timeout},
		fsckEvery: fsckEvery, workers: workers, now: now, logger: logger, repos: map[string]*Repo{},
		materialized: reg.Counter("origo_repo_materialized_total", "repositories built from the log onto an empty disk"),
		applied:      reg.Counter("origo_repo_entries_applied_total", "log entries applied to local copies"),
		rebuilt:      reg.Counter("origo_repo_rebuilt_total", "local copies removed as corrupt and rebuilt"),
		materialize:  reg.Histogram("origo_repo_materialize_seconds", "time to bring a local copy current", []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60}),
	}
	for _, m := range []*metrics.Counter{c.materialized, c.applied, c.rebuilt} {
		m.Add(nil, 0) // the series reads 0 before the first event
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
			return nil
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
// ErrDeleted are the two refusals a handler turns into a 404.
func (c *Cache) Acquire(ctx context.Context, id string, write bool) (*Repo, func(), error) {
	if !wal.ValidID(id) {
		return nil, nil, ErrNotFound
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
	err := c.sync(ctx, r, write)
	if errors.Is(err, errNeedsWrite) {
		// A reader found the copy behind: upgrade, catch up, downgrade.
		r.lock.RUnlock()
		r.lock.Lock()
		err = c.sync(ctx, r, true)
		r.lock.Unlock()
		r.lock.RLock()
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	return r, release, nil
}

var errNeedsWrite = errors.New("repo: catch-up needs the write lock")

// sync runs the currency check and applies what the local copy lacks.
// Without the write lock it only reports whether work is needed.
func (c *Cache) sync(ctx context.Context, r *Repo, write bool) error {
	if r.Index != nil && r.Local && write && c.fsckEvery > 0 {
		r.opens++
		if r.opens%c.fsckEvery == 0 {
			if err := c.Verify(ctx, r); err != nil {
				return err
			}
		}
	}
	newest, changed, err := c.log.Newest(ctx, r.ID, r.Seq, r.Local)
	if err != nil {
		if errors.Is(err, wal.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if !changed && r.Index == nil {
		// A local copy from a previous process: its index object is
		// read once so a commit has a base.
		if !write {
			return errNeedsWrite
		}
		ix, err := c.log.ReadIndex(ctx, r.ID, r.Seq)
		if err != nil {
			return err
		}
		r.Index = ix
	}
	if !changed {
		if r.Index.DeletedAt != nil {
			return ErrDeleted
		}
		return nil
	}
	if !write {
		return errNeedsWrite
	}
	if newest.DeletedAt != nil {
		c.evict(r)
		r.Index = newest
		return ErrDeleted
	}
	return c.Apply(ctx, r, newest)
}

// Apply brings the local copy to ix: packs first, then every entry above
// the local sequence, then the reference map reconciled to the index so
// the result equals the index whatever the copy held before. The caller
// holds the write lock; a push handler calls it with the index another
// writer committed while the push was in flight.
func (c *Cache) Apply(ctx context.Context, r *Repo, ix *wal.Index) error {
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
			return err
		}
	}
	var pending []wal.IndexEntry
	for _, e := range ix.Entries {
		if e.Seq > r.Seq {
			pending = append(pending, e)
		}
	}
	if err := c.applyEntries(ctx, r, pending); err != nil {
		if IsCorruption(err) {
			c.rebuilt.Inc(nil)
			c.evict(r)
			return fmt.Errorf("%w: %w", ErrCorrupt, err)
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

// applyEntries fetches and indexes the entries with concurrent workers
// (spec 005, materialization budget): each worker takes the next entry
// in sequence order, and the reference transactions are not applied
// per entry, because reconcileRefs sets the whole map once at the end.
// A thin pack whose base is in a lower entry that has not landed yet
// fails its index-pack; the worker then waits until every lower entry
// has landed and runs it once more, and a failure after that is
// corruption. The first error stops every worker.
func (c *Cache) applyEntries(ctx context.Context, r *Repo, entries []wal.IndexEntry) error {
	if len(entries) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		cond     = sync.NewCond(&mu)
		landed   = make([]bool, len(entries))
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
		cond.Broadcast()
	}
	// lowerLanded blocks until every entry before position i has
	// landed, or a worker failed.
	lowerLanded := func(i int) error {
		mu.Lock()
		defer mu.Unlock()
		for {
			if firstErr != nil {
				return firstErr
			}
			done := true
			for _, ok := range landed[:i] {
				if !ok {
					done = false
					break
				}
			}
			if done {
				return nil
			}
			cond.Wait()
		}
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(c.workers, len(entries)) {
		wg.Go(func() {
			for i := range next {
				if err := c.applyEntry(ctx, r, entries[i], func() error { return lowerLanded(i) }); err != nil {
					fail(err)
					return
				}
				mu.Lock()
				landed[i] = true
				mu.Unlock()
				cond.Broadcast()
				c.applied.Inc(nil)
			}
		})
	}
	for i := range entries {
		select {
		case next <- i:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(next)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
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
	// Objects from a client are checked, the local copy is never garbage
	// collected on its own (compaction is a log entry, spec 006), and the
	// capabilities spec 003 promises are advertised.
	for _, kv := range [][2]string{
		{"core.protectNTFS", "true"},
		{"receive.fsckObjects", "true"},
		{"receive.advertiseAtomic", "true"},
		{"receive.advertisePushOptions", "true"},
		{"receive.autogc", "false"},
		{"gc.auto", "0"},
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.allowAnySHA1InWant", "true"},
	} {
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
		rc, _, err := c.log.Store().Get(ctx, c.log.RepoPrefix(r.ID)+k, "")
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

// applyEntry fetches one entry, verifies its pack, and indexes the pack
// into the object store. Its reference transaction is not applied:
// Apply reconciles the whole map once. lowerLanded waits for every
// lower entry, for the retry of a thin pack.
func (c *Cache) applyEntry(ctx context.Context, r *Repo, e wal.IndexEntry, lowerLanded func() error) error {
	rc, _, err := c.log.Store().Get(ctx, c.log.RepoPrefix(r.ID)+e.Key, "")
	if err != nil {
		return fmt.Errorf("repo: entry %s: %w", e.Key, err)
	}
	defer func() { _ = rc.Close() }()
	h, _, pack, err := wal.ReadEntryHead(rc)
	if err != nil {
		return err
	}
	if h.Seq != e.Seq || h.Kind != e.Kind {
		return fmt.Errorf("repo: entry %s carries seq %d kind %s", e.Key, h.Seq, h.Kind)
	}
	if h.PackBytes == 0 {
		return nil
	}
	return c.indexPack(ctx, r, pack, h.PackBytes, h.PackSHA256, lowerLanded)
}

// missingBase is what index-pack prints when a thin pack needs an
// object the store does not hold yet.
const missingBase = "did not receive expected object"

// indexPack spools the pack, checks its digest, and runs index-pack with
// --fix-thin so a thin pack from a push completes against the local
// objects. Git writes the completed pack, its index, and its reverse
// index beside a path in the spool, and the three are renamed into
// objects/pack under the content name git printed, index first, so no
// worker of another entry sees a pack without its index and a failed
// run leaves nothing but a file in the spool. A run that lacks a base
// is repeated once after every lower entry landed.
func (c *Cache) indexPack(ctx context.Context, r *Repo, pack io.Reader, size int64, sum string, lowerLanded func() error) error {
	f, err := os.CreateTemp(c.SpoolDir(), "entry-*.pack")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(pack, size+1))
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sum {
		return fmt.Errorf("repo: pack of %d bytes with digest %s does not match the entry (%d bytes, %s)", n, hex.EncodeToString(h.Sum(nil)), size, sum)
	}
	err = c.indexOnce(ctx, r, f)
	var ge *Error
	if errors.As(err, &ge) && strings.Contains(ge.Stderr, missingBase) {
		if err := lowerLanded(); err != nil {
			return err
		}
		c.thinRetries.Add(1)
		err = c.indexOnce(ctx, r, f)
	}
	return err
}

// indexOnce runs index-pack over the spooled pack once and moves the
// result into objects/pack.
func (c *Cache) indexOnce(ctx context.Context, r *Repo, f *os.File) error {
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
		if err := os.Rename(base+ext, filepath.Join(dir, "pack-"+name+ext)); err != nil && !(ext == ".rev" && errors.Is(err, os.ErrNotExist)) {
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
