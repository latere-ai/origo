// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/events"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/repo"
	"github.com/latere-ai/origo/internal/wal"
)

// The import of spec 019: one existing repository brought in with its
// history as one log entry. The node mirrors the source through the
// egress proxy of spec 016, repacks, uploads the packs, and commits an
// entry in the shape of a compaction, so every other node materializes
// it by downloading packs.

// The bounds of one import.
const (
	// DefaultImportTimeout bounds one whole run: the clone, the repack,
	// the connectivity check, the uploads, and the commit.
	DefaultImportTimeout = 30 * time.Minute
	// ImportLease is how long an importing_since whose node is out of
	// the live set is honoured before another node clears it.
	ImportLease = 45 * time.Minute
)

// The import states GET /v1/repos/{id}/import answers with.
const (
	ImportRunning = "running"
	ImportDone    = "done"
	ImportFailed  = "failed"
)

// The import_error values this node writes for a lease it did not
// finish itself.
const (
	ErrorNodeLost      = "import node lost"
	ErrorNodeRestarted = "import node restarted"
	ErrorNotEmpty      = "not empty"
)

// KindImported is the event a finished import emits.
const KindImported = "imported"

// Members is the live set of spec 005 as the import lease reads it:
// only a node holding the gossip secret enters it, so a name outside it
// is a node that is gone. A nil Members is this node alone.
type Members interface {
	Live() []string
}

type importRequest struct {
	Source string `json:"source"`
	Token  string `json:"token"`
}

// ImportState is what GET /v1/repos/{id}/import answers, read out of
// meta so any node answers for an import running on another.
type ImportState struct {
	State      string     `json:"state"`
	Refs       int        `json:"refs"`
	Bytes      int64      `json:"bytes"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Error      string     `json:"error,omitempty"`
}

// importDir is where one import's mirror lives while it runs, under
// ORIGO_DATA_DIR/spool/. The directory is named by the repository, so a
// node that restarts finds the leases it left by listing the root.
func (h *Handler) importRoot() string { return filepath.Join(h.cache.SpoolDir(), "import") }

func (h *Handler) importDir(id string) string { return filepath.Join(h.importRoot(), id) }

// source validates the URL a caller named: https only, and a host the
// egress rules of spec 016 admit, checked before any connection.
func (h *Handler) source(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, &readError{status: http.StatusBadRequest, code: contract.CodeInvalid,
			details: map[string]any{"reason": "source must be an absolute https URL", "field": "source"}}
	}
	if u.Scheme != "https" {
		return nil, &readError{status: http.StatusBadRequest, code: contract.CodeInvalid,
			details: map[string]any{"reason": "source must be an https URL", "field": "source"}}
	}
	if _, err := h.egress.Resolve(ctx, u.Hostname()); err != nil {
		ee, ok := errors.AsType[*EgressError](err)
		if !ok {
			return nil, err
		}
		return nil, &readError{status: http.StatusBadRequest, code: contract.CodeInvalid, details: ee.Details()}
	}
	return u, nil
}

// startImport takes the lease and starts the run in the background. The
// answer is 202 at once: an import of a large repository runs for
// minutes and no client holds a request open for it.
func (h *Handler) startImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := decode(r, &req); err != nil {
		invalid(w, "body: "+err.Error(), "")
		return
	}
	m, ix, decision, ok := h.loadDecision(w, r, auth.ActionAdmin, false)
	if !ok {
		return
	}
	u, err := h.source(r.Context(), req.Source)
	if err != nil {
		writeReadError(w, err)
		return
	}
	// A repository with history is not a target: the entry an import
	// commits folds every entry before it, which would drop that
	// history from the log.
	if len(ix.Entries) > 0 {
		contract.Write(w, http.StatusConflict, contract.CodeRepoNotEmpty, map[string]any{"seq": ix.Seq})
		return
	}
	h.expireLease(r.Context(), m)
	if m.ImportingSince != nil {
		contract.Write(w, http.StatusConflict, contract.CodeRepoImporting, map[string]any{"started_at": m.ImportingSince})
		return
	}
	at := h.now().UTC()
	m.ImportingSince, m.ImportNode = &at, h.node
	m.ImportError, m.ImportedAt, m.ImportRefs, m.ImportBytes = "", nil, 0, 0
	if err := h.log.WriteMeta(r.Context(), m); err != nil {
		h.storageError(w, r, err)
		return
	}
	// The run outlives the request, so everything it needs is read off
	// the request before it returns: net/http reuses the request once
	// the handler is done.
	by, base := pusher(r), context.WithoutCancel(r.Context())
	h.imports.Go(func() {
		ctx, cancel := context.WithTimeout(base, h.importTimeout)
		defer cancel()
		h.runImport(ctx, m.ID, u, req.Token, ix, quotaOf(decision), by)
	})
	httpjson.Write(w, http.StatusAccepted, ImportState{State: ImportRunning, StartedAt: &at})
}

// quotaOf is the authorizer's quota_bytes for the caller, the default
// when it named none (spec 012).
func quotaOf(d auth.Decision) int64 {
	if d.QuotaBytes <= 0 {
		return limits.DefaultQuotaBytes
	}
	return d.QuotaBytes
}

// importState answers the state of the import of one repository.
func (h *Handler) importState(w http.ResponseWriter, r *http.Request) {
	m, _, ok := h.load(w, r, auth.ActionRead, false)
	if !ok {
		return
	}
	h.expireLease(r.Context(), m)
	out := ImportState{Refs: m.ImportRefs, Bytes: m.ImportBytes, StartedAt: m.ImportingSince, FinishedAt: m.ImportedAt, Error: m.ImportError}
	switch {
	case m.ImportingSince != nil:
		out.State = ImportRunning
	case m.ImportError != "":
		out.State = ImportFailed
	case m.ImportedAt != nil:
		out.State = ImportDone
	default:
		contract.Write(w, http.StatusNotFound, contract.CodeImportNotFound, map[string]any{"id": m.ID})
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

// expireLease clears a lease whose node is gone: an importing_since
// older than ImportLease whose import_node is not in the live set is a
// node that died mid-import, and the repository accepts a new import
// once it is cleared. A clearing write the bucket refuses leaves the
// lease where it was, so m still reads running.
func (h *Handler) expireLease(ctx context.Context, m *wal.Meta) {
	if m.ImportingSince == nil {
		return
	}
	if h.now().Sub(*m.ImportingSince) < ImportLease || h.isLive(m.ImportNode) {
		return
	}
	h.clearLease(ctx, m, ErrorNodeLost)
}

// isLive reports whether the node is in the live set of spec 005. A
// handler with no membership is a single node, where only its own name
// is live.
func (h *Handler) isLive(node string) bool {
	if node == "" {
		return false
	}
	if h.members == nil {
		return node == h.node
	}
	return slices.Contains(h.members.Live(), node)
}

// clearLease drops the lease and records why, so the repository reports
// failed and accepts a new import.
func (h *Handler) clearLease(ctx context.Context, m *wal.Meta, reason string) {
	next := *m
	next.ImportingSince, next.ImportNode, next.ImportError = nil, "", reason
	// The write is the record: a bucket that refuses it leaves the
	// lease where it was, and the caller sees running until the next
	// read clears it.
	if err := h.log.WriteMeta(ctx, &next); err != nil {
		h.logger.WarnContext(ctx, "import lease not cleared", "repo", m.ID, "reason", reason, "error", err)
		return
	}
	*m = next
}

// ClearImportLeases is the start-up sweep of spec 019: a node that
// restarts under its own name frees every repository it was importing
// at once, rather than after ImportLease. The scratch directories are
// where a running import leaves its mirror, so they are the list.
func (h *Handler) ClearImportLeases(ctx context.Context) error {
	entries, err := os.ReadDir(h.importRoot())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		id := e.Name()
		if !wal.ValidID(id) {
			continue
		}
		m, err := h.log.ReadMeta(ctx, id)
		if err == nil && m.ImportNode == h.node && m.ImportingSince != nil {
			h.clearLease(ctx, m, ErrorNodeRestarted)
		} else if err != nil && !errors.Is(err, wal.ErrNotFound) {
			errs = append(errs, err)
		}
		if err := os.RemoveAll(h.importDir(id)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runImport is the whole procedure, under one deadline. Nothing is
// committed when any step fails: the scratch directory is removed and
// import_error says which step it was.
func (h *Handler) runImport(ctx context.Context, id string, source *url.URL, token string, base *wal.Index, quota int64, by events.Pusher) {
	dir := h.importDir(id)
	defer func() { _ = os.RemoveAll(dir) }()
	refs, bytes, err := h.mirror(ctx, id, source, token, base, quota, dir)
	m, rerr := h.log.ReadMeta(ctx, id)
	if rerr != nil {
		h.logger.ErrorContext(ctx, "import finished but meta is unreadable", "repo", id, "error", rerr)
		return
	}
	m.ImportingSince, m.ImportNode = nil, ""
	if err != nil {
		m.ImportError = importReason(err)
		h.logger.WarnContext(ctx, "import failed", "repo", id, "source", redact(source), "error", err)
		if werr := h.log.WriteMeta(ctx, m); werr != nil {
			h.logger.ErrorContext(ctx, "import failure not recorded", "repo", id, "error", werr)
		}
		return
	}
	at := h.now().UTC()
	m.ImportError, m.ImportedAt, m.ImportRefs, m.ImportBytes = "", &at, refs, bytes
	if werr := h.log.WriteMeta(ctx, m); werr != nil {
		h.logger.ErrorContext(ctx, "import result not recorded", "repo", id, "error", werr)
		return
	}
	h.logger.InfoContext(ctx, "imported", "repo", id, "source", redact(source), "refs", refs, "bytes", bytes)
	if err := h.events.Emit(ctx, id, KindImported, at, by, map[string]any{"source": redact(source), "refs": refs, "bytes": bytes}); err != nil {
		h.logger.WarnContext(ctx, "event not emitted", "repo", id, "kind", KindImported, "error", err)
	}
}

// redact is the source in an event and a log line: the host and the
// path, never the credentials a URL can carry and never the bearer.
func redact(u *url.URL) string {
	c := *u
	c.User, c.RawQuery, c.Fragment = nil, "", ""
	return c.String()
}

// importReason is the developer sentence import_error carries. A git
// failure carries its own stderr, which is the source's message.
func importReason(err error) string {
	var ge *repo.Error
	if errors.As(err, &ge) && ge.Stderr != "" {
		return ge.Stderr
	}
	return err.Error()
}

// mirror clones, repacks, proves connectivity, uploads the packs, and
// commits the entry. It answers the reference count and the pack bytes.
func (h *Handler) mirror(ctx context.Context, id string, source *url.URL, token string, base *wal.Index, quota int64, dir string) (int, int64, error) {
	if err := os.MkdirAll(h.importRoot(), 0o700); err != nil {
		return 0, 0, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return 0, 0, err
	}
	proxy, err := h.egress.StartProxy(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer proxy.Close()
	// The source URL git is given is http:// and the proxy dials the
	// https source itself (spec 016); the bearer and the proxy
	// credential travel in the environment, so no process listing and
	// no log line carries either.
	rewritten, env := proxy.GitConfig(source, token)
	git := h.cache.Git()
	run := func(dir string, args ...string) error {
		cmd := git.Command(ctx, dir, args...)
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return &repo.Error{Args: args, Err: err, Stderr: strings.TrimSpace(string(out))}
		}
		return nil
	}
	// transfer.fsckObjects is no secret, so it goes on the command line
	// where a process listing shows the check is on.
	if err := run(h.importRoot(), "-c", "transfer.fsckObjects=true", "clone", "--mirror", "--end-of-options", rewritten, dir); err != nil {
		if ee := proxy.Refusal(); ee != nil {
			return 0, 0, ee
		}
		return 0, 0, err
	}
	if err := run(dir, "repack", "-a", "-d"); err != nil {
		return 0, 0, err
	}
	if err := run(dir, "fsck", "--connectivity-only", "--no-progress"); err != nil {
		return 0, 0, err
	}

	keys, bytes, err := h.packBytes(dir)
	if err != nil {
		return 0, 0, err
	}
	q, err := h.limits.Measure(ctx, id, 0, bytes, quota)
	if err != nil {
		return 0, 0, err
	}
	if q.Over() {
		return 0, 0, fmt.Errorf("over quota: %d bytes, limit %d", q.Bytes, q.Max)
	}
	transaction, err := h.transaction(ctx, dir, base)
	if err != nil {
		return 0, 0, err
	}
	if err := h.uploadPacks(ctx, id, dir, keys); err != nil {
		return 0, 0, err
	}
	// The entry has the shape of a compaction: no pack of its own, the
	// uploaded packs, and compacted_through the sequence before it, so
	// the index after it lists the packs and this one entry.
	entry := wal.Entry{
		Kind: wal.KindCompact, Subject: "", Refs: transaction,
		Packs: keys, PacksBytes: bytes, CompactedThrough: base.Seq,
	}
	// A second import that started in the same window loses its round
	// and refuses rather than replaying, so the log never holds two.
	_, err = h.log.Commit(ctx, id, base, entry, func(context.Context, *wal.Index) error {
		return errors.New(ErrorNotEmpty)
	})
	if err != nil {
		return 0, 0, err
	}
	return len(transaction), bytes, nil
}

// packBytes lists the packs the repack left and sums their .pack bytes,
// the figure the entry records as the index's size_bytes.
func (h *Handler) packBytes(dir string) ([]string, int64, error) {
	files, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(files)
	keys := make([]string, 0, len(files))
	var total int64
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			return nil, 0, err
		}
		total += info.Size()
		keys = append(keys, wal.PackKey(filepath.Base(f)))
	}
	if len(keys) == 0 {
		return nil, 0, errors.New("the source has no objects")
	}
	return keys, total, nil
}

// uploadPacks puts every pack under the key the mapping of spec 004
// gives its file name, the .idx before the .pack so a reader never
// sees a pack without one.
func (h *Handler) uploadPacks(ctx context.Context, id, dir string, keys []string) error {
	for _, key := range keys {
		base := strings.TrimSuffix(key, ".pack")
		for _, ext := range []string{".idx", ".pack"} {
			from := filepath.Join(dir, "objects", "pack", wal.PackFile(base)+ext)
			body, err := wal.FileBody(from)
			if err != nil {
				return err
			}
			if _, err := h.log.Store().Put(ctx, h.log.RepoPrefix(id)+base+ext, body); err != nil {
				return fmt.Errorf("import: upload %s: %w", base+ext, err)
			}
		}
	}
	return nil
}

// transaction is the reference transaction of the import entry: every
// reference of the mirror created from zeros, and HEAD moved to the
// source's symbolic target unless index/0 already holds it, because a
// transaction never names a reference that does not change.
func (h *Handler) transaction(ctx context.Context, dir string, base *wal.Index) ([]wal.RefUpdate, error) {
	git := h.cache.Git()
	out, err := git.Run(ctx, dir, nil, "show-ref")
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var refs []wal.RefUpdate
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if !wal.ValidRefName(name) {
			return nil, fmt.Errorf("the source has a reference Origo cannot record: %q", name)
		}
		refs = append(refs, wal.RefUpdate{Ref: name, Old: wal.ZeroSHA, New: sha})
	}
	if len(refs) == 0 {
		return nil, errors.New("the source has no references")
	}
	head, err := git.Run(ctx, dir, nil, "symbolic-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	target := "ref: " + strings.TrimSpace(string(head))
	if old := base.Refs["HEAD"]; old != target {
		refs = append(refs, wal.RefUpdate{Ref: "HEAD", Old: old, New: target})
	}
	if err := wal.ValidateTransaction(refs); err != nil {
		return nil, err
	}
	return refs, nil
}
