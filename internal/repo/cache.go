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
	"time"

	"github.com/latere-ai/origo/internal/metrics"
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
	Logger    *slog.Logger
	Metrics   *metrics.Registry
}

// Cache holds the materialized repositories of one node.
type Cache struct {
	dir       string
	log       *wal.Log
	git       *Git
	fsckEvery int
	logger    *slog.Logger

	mu    sync.Mutex
	repos map[string]*Repo

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
		reg = metrics.New()
	}
	fsckEvery := o.FsckEvery
	if fsckEvery == 0 {
		fsckEvery = 256
	}
	return &Cache{
		dir: o.Dir, log: o.Log, git: &Git{Bin: path, Home: home, Timeout: timeout},
		fsckEvery: fsckEvery, logger: logger, repos: map[string]*Repo{},
		materialized: reg.Counter("origo_repo_materialized_total", "repositories built from the log onto an empty disk"),
		applied:      reg.Counter("origo_repo_entries_applied_total", "log entries applied to local copies"),
		rebuilt:      reg.Counter("origo_repo_rebuilt_total", "local copies removed as corrupt and rebuilt"),
		materialize:  reg.Histogram("origo_repo_materialize_seconds", "time to bring a local copy current", []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60}),
	}, nil
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
	if !changed {
		if r.Index == nil {
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
		c.materialized.Inc()
	}
	if fresh || r.Seq < ix.CompactedThrough {
		for _, p := range ix.Packs {
			if err := c.fetchPack(ctx, r, p); err != nil {
				return err
			}
		}
	}
	for _, e := range ix.Entries {
		if e.Seq <= r.Seq {
			continue
		}
		if err := c.applyEntry(ctx, r, e); err != nil {
			if IsCorruption(err) {
				c.rebuilt.Inc()
				c.evict(r)
				return fmt.Errorf("%w: %w", ErrCorrupt, err)
			}
			return err
		}
		c.applied.Inc()
	}
	if err := c.reconcileRefs(ctx, r, ix); err != nil {
		return err
	}
	if err := c.writeState(r, ix.Seq); err != nil {
		return err
	}
	r.Seq, r.Local, r.Index = ix.Seq, true, ix
	c.materialize.Observe(time.Since(start).Seconds())
	return nil
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

// fetchPack downloads a compaction pack and its index into objects/pack.
func (c *Cache) fetchPack(ctx context.Context, r *Repo, key string) error {
	name := filepath.Base(key)
	dst := filepath.Join(r.Dir, "objects", "pack", name)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	for _, suffix := range []string{".idx", ""} {
		k := strings.TrimSuffix(key, ".pack") + suffix
		if suffix == "" {
			k = key
		}
		rc, _, err := c.log.Store().Get(ctx, c.log.RepoPrefix(r.ID)+k, "")
		if err != nil {
			return fmt.Errorf("repo: pack %s: %w", k, err)
		}
		err = writeAtomic(filepath.Join(r.Dir, "objects", "pack", filepath.Base(k)), rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// applyEntry fetches one entry, verifies its pack, indexes the pack into
// the object store, and applies its reference transaction.
func (c *Cache) applyEntry(ctx context.Context, r *Repo, e wal.IndexEntry) error {
	rc, _, err := c.log.Store().Get(ctx, c.log.RepoPrefix(r.ID)+e.Key, "")
	if err != nil {
		return fmt.Errorf("repo: entry %s: %w", e.Key, err)
	}
	defer func() { _ = rc.Close() }()
	h, refs, pack, err := wal.ReadEntryHead(rc)
	if err != nil {
		return err
	}
	if h.Seq != e.Seq || h.Kind != e.Kind {
		return fmt.Errorf("repo: entry %s carries seq %d kind %s", e.Key, h.Seq, h.Kind)
	}
	if h.PackBytes > 0 {
		if err := c.indexPack(ctx, r, pack, h.PackBytes, h.PackSHA256); err != nil {
			return err
		}
	}
	return c.updateRefs(ctx, r, refs)
}

// indexPack spools the pack, checks its digest, and runs index-pack with
// --fix-thin so a thin pack from a push completes against the local
// objects. The pack lands in objects/pack under its content name.
func (c *Cache) indexPack(ctx context.Context, r *Repo, pack io.Reader, size int64, sum string) error {
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
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = c.git.Run(ctx, r.Dir, f, "index-pack", "--stdin", "--fix-thin", "--strict")
	return err
}

// updateRefs applies a transaction. Values are set, not compared: the log
// is the authority and a replay after a partial apply must converge.
func (c *Cache) updateRefs(ctx context.Context, r *Repo, refs []wal.RefUpdate) error {
	var script bytes.Buffer
	for _, u := range refs {
		if u.Ref == "HEAD" {
			if _, err := c.git.Run(ctx, r.Dir, nil, "symbolic-ref", "HEAD", strings.TrimPrefix(u.New, "ref: ")); err != nil {
				return err
			}
			continue
		}
		if u.New == wal.ZeroSHA {
			fmt.Fprintf(&script, "delete %s\n", u.Ref)
		} else {
			fmt.Fprintf(&script, "update %s %s\n", u.Ref, u.New)
		}
	}
	if script.Len() == 0 {
		return nil
	}
	_, err := c.git.Run(ctx, r.Dir, &script, "update-ref", "--stdin")
	return err
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
		c.rebuilt.Inc()
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
