// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/repo"
)

// The verification of spec 014: the migration's proof that Origo's copy
// of a repository is the source's. It reads both sides and writes
// nothing to either, so an operator runs it before a cut-over, again
// after the prior host froze its copy, and as often as a doubt asks
// for it. It is a POST because the source's bearer travels in the body,
// where no log line and no access record carries it.

// KindVerified is the event a finished verification emits.
const KindVerified = "verified"

type verifyRequest struct {
	Source string `json:"source"`
	Token  string `json:"token"`
}

// RefDifference is one reference the two sides disagree on: a hash each,
// with the missing side empty when only one side holds it.
type RefDifference struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Origo  string `json:"origo"`
}

// VerifyRefs is the reference half of the result: how many each side
// holds and every one they disagree on.
type VerifyRefs struct {
	Source    int             `json:"source"`
	Origo     int             `json:"origo"`
	Differing []RefDifference `json:"differing"`
}

// VerifyObjects is the object half: the reachable objects of Origo's
// copy. Nothing is counted on the source, because a count over a remote
// needs a clone; the operator compares this with the prior host's own
// figure.
type VerifyObjects struct {
	Origo int64 `json:"origo"`
}

// VerifyResult is what POST /v1/repos/{id}/verify answers.
type VerifyResult struct {
	Equal     bool          `json:"equal"`
	Refs      VerifyRefs    `json:"refs"`
	Objects   VerifyObjects `json:"objects"`
	CheckedAt time.Time     `json:"checked_at"`
}

// verify compares the source with this node's copy and records the
// verdict in meta. Both sides are read under the request's read budget
// (spec 009), the source through the egress proxy of spec 016 the way
// the import of spec 019 fetches.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := decode(r, &req); err != nil {
		invalid(w, "body: "+err.Error(), "")
		return
	}
	m, _, ok := h.load(w, r, auth.ActionAdmin, false)
	if !ok {
		return
	}
	u, err := h.source(r.Context(), req.Source)
	if err != nil {
		writeReadError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.readTimeout)
	defer cancel()
	sourceRefs, err := h.sourceRefs(ctx, u, req.Token)
	if err != nil {
		writeReadError(w, err)
		return
	}
	localRefs, objects, err := h.localRefs(ctx, m.ID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrDeleted) {
			h.notFoundOrGone(w, r, m.ID)
			return
		}
		h.storageError(w, r, err)
		return
	}
	result := VerifyResult{
		Refs:      compareRefs(sourceRefs, localRefs),
		Objects:   VerifyObjects{Origo: objects},
		CheckedAt: h.now().UTC(),
	}
	result.Equal = len(result.Refs.Differing) == 0

	// The verdict is the state origod migrate resumes from, so it is
	// written before the answer and before the event.
	m.VerifiedAt, m.VerifiedEqual = &result.CheckedAt, &result.Equal
	if err := h.log.WriteMeta(r.Context(), m); err != nil {
		h.storageError(w, r, err)
		return
	}
	h.logger.InfoContext(ctx, "verified", "repo", m.ID, "source", redact(u),
		"equal", result.Equal, "source_refs", result.Refs.Source, "origo_refs", result.Refs.Origo, "objects", objects)
	h.emit(r, m.ID, KindVerified, result.CheckedAt, map[string]any{
		"equal": result.Equal, "refs": result.Refs, "objects": result.Objects, "checked_at": result.CheckedAt,
	})
	httpjson.Write(w, http.StatusOK, result)
}

// verifyDir is the scratch directory a source read runs in: git needs a
// working directory and this one holds no repository, so no
// configuration of a copy reaches the subprocess.
func (h *Handler) verifyDir() string { return filepath.Join(h.cache.SpoolDir(), "verify") }

// sourceRefs is git ls-remote against the source through the egress
// proxy: the bearer and the proxy travel in the environment, so no
// process listing and no log line carries either, and
// transfer.fsckObjects goes on the command line, where it is no secret
// (spec 016).
func (h *Handler) sourceRefs(ctx context.Context, source *url.URL, token string) (map[string]string, error) {
	if err := os.MkdirAll(h.verifyDir(), 0o700); err != nil {
		return nil, err
	}
	proxy, err := h.egress.StartProxy(ctx)
	if err != nil {
		return nil, err
	}
	defer proxy.Close()
	rewritten, env := proxy.GitConfig(source, token)
	cmd := h.cache.Git().Command(ctx, h.verifyDir(), "-c", "transfer.fsckObjects=true", "ls-remote", "--end-of-options", rewritten)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	if err != nil {
		if ee := proxy.Refusal(); ee != nil {
			return nil, &readError{status: http.StatusBadRequest, code: contract.CodeInvalid, details: ee.Details()}
		}
		h.logger.WarnContext(ctx, "the source did not answer ls-remote", "source", redact(source), "error", err)
		return nil, &readError{status: http.StatusBadRequest, code: contract.CodeInvalid, details: map[string]any{
			"reason": "the source did not answer ls-remote", "field": "source", "host": source.Hostname(),
		}}
	}
	return parseLsRemote(string(out)), nil
}

// parseLsRemote reads the advertisement into a map of name to hash. A
// peeled line, <ref>^{}, names a tag's target and not a reference, and
// HEAD is a symbolic reference whose value on the other side is a name
// rather than a hash; neither is comparable, so both are dropped.
func parseLsRemote(out string) map[string]string {
	refs := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || name == "HEAD" || strings.HasSuffix(name, "^{}") {
			continue
		}
		refs[name] = sha
	}
	return refs
}

// localRefs is git for-each-ref and the reachable-object count of this
// node's copy, taken under a read lock after the currency check so the
// comparison is against the log's newest state.
func (h *Handler) localRefs(ctx context.Context, id string) (map[string]string, int64, error) {
	rp, release, err := h.cache.Acquire(ctx, id, false)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	git := h.cache.Git()
	out, err := git.Run(ctx, rp.Dir, nil, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, 0, err
	}
	refs := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		refs[name] = sha
	}
	objects, err := git.Run(ctx, rp.Dir, nil, "rev-list", "--objects", "--all")
	if err != nil {
		return nil, 0, err
	}
	return refs, countLines(objects), nil
}

// countLines is the reachable-object count: one object per line of
// rev-list --objects --all.
func countLines(out []byte) int64 {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return int64(strings.Count(trimmed, "\n") + 1)
}

// compareRefs is the comparison: every name on either side, with the
// pairs that differ listed in name order so two runs of one state
// answer the same document.
func compareRefs(source, origo map[string]string) VerifyRefs {
	out := VerifyRefs{Source: len(source), Origo: len(origo), Differing: []RefDifference{}}
	names := map[string]bool{}
	for name := range source {
		names[name] = true
	}
	for name := range origo {
		names[name] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		if source[name] != origo[name] {
			out.Differing = append(out.Differing, RefDifference{Name: name, Source: source[name], Origo: origo[name]})
		}
	}
	return out
}
