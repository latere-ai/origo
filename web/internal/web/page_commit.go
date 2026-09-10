// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/latere-ai/origo-web/internal/diff"
	"github.com/latere-ai/origo-web/internal/origo"
)

type commitData struct {
	View      view
	Repo      *repoView
	Commit    commitView
	Trailers  []origo.Trailer
	Parents   []parentLink
	Stats     *origo.Stats
	Patch     diff.Patch
	Files     []fileView
	Root      bool
	Single    string
	PatchURL  string
	TreeURL   string
	Truncated bool
	Stale     string
}

type parentLink struct {
	Short string
	URL   string
}

// fileView is one file of a diff with the links its state needs.
type fileView struct {
	diff.File
	ID      string
	OnlyURL string
	BlobURL string
	LogURL  string
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "log")
	if !ok {
		return
	}
	sha := r.PathValue("sha")

	commit, meta, err := s.api.Commit(r.Context(), rc.tok, rc.repo.ID, sha)
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	rc.v.Title = commit.Subject()
	data := commitData{
		View:     rc.v,
		Repo:     rc.rv,
		Commit:   newCommitView(commit, rc.rv.URL()),
		Trailers: commit.Trailers,
		Stats:    commit.Stats,
		PatchURL: rc.rv.URL() + "/patch/" + url.PathEscape(commit.SHA),
		TreeURL:  rc.rv.URL() + "/tree/?ref=" + url.QueryEscape(commit.SHA),
		Stale:    firstNonEmpty(rc.stale, staleSentence(meta)),
		Single:   r.URL.Query().Get("path"),
	}
	for _, p := range commit.Parents {
		data.Parents = append(data.Parents, parentLink{Short: origo.Short(p), URL: rc.rv.URL() + "/commit/" + url.PathEscape(p)})
	}

	// A commit with no parent has nothing to be compared against. It is a
	// state, not a failure: the screen says so and offers the tree.
	if len(commit.Parents) == 0 {
		data.Root = true
		s.render(w, r, http.StatusOK, "commit", data)
		return
	}

	base := commit.Parents[0]
	text, dm, err := s.api.Compare(r.Context(), rc.tok, rc.repo.ID, base, commit.SHA, data.Single)
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	budget := diff.DefaultBudget
	if data.Single != "" {
		budget.Force = data.Single
	}
	data.Patch = diff.Parse(text, budget)
	data.Patch.Truncated = dm.Truncated
	data.Truncated = dm.Truncated
	data.Files = fileViews(rc.rv, data.Patch.Files, rc.rv.URL()+"/commit/"+url.PathEscape(commit.SHA), commit.SHA)
	s.render(w, r, http.StatusOK, "commit", data)
}

func fileViews(rv *repoView, files []diff.File, selfURL, rev string) []fileView {
	out := make([]fileView, 0, len(files))
	for i, f := range files {
		v := fileView{File: f, ID: "f" + itoa(i+1)}
		sep := "?"
		if strings.Contains(selfURL, "?") {
			sep = "&"
		}
		v.OnlyURL = selfURL + sep + "path=" + url.QueryEscape(f.Path)
		if f.Status != "deleted" {
			v.BlobURL = rv.URL() + "/blob/" + escapePath(f.Path) + "?ref=" + url.QueryEscape(rev)
		}
		v.LogURL = rv.URL() + "/log?ref=" + url.QueryEscape(rev) + "&path=" + url.QueryEscape(f.Path)
		out = append(out, v)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// handlePatch serves the diff as the bytes Origo produced, so a reader can
// pipe it into git apply. It is the same call the commit screen makes.
func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "log")
	if !ok {
		return
	}
	sha := r.PathValue("sha")
	commit, _, err := s.api.Commit(r.Context(), rc.tok, rc.repo.ID, sha)
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	if len(commit.Parents) == 0 {
		s.notFound(w, r, rc.v)
		return
	}
	text, _, err := s.api.Compare(r.Context(), rc.tok, rc.repo.ID, commit.Parents[0], commit.SHA, "")
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+origo.Short(commit.SHA)+".patch\"")
	_, _ = w.Write(text)
}

type compareData struct {
	View      view
	Repo      *repoView
	Base      string
	Head      string
	Patch     diff.Patch
	Files     []fileView
	Truncated bool
	Single    string
	Stale     string
	Ready     bool
}

// handleCompare is the same diff view between any two revisions, and the one
// screen that starts as a form: with no revisions named it asks for two.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "log")
	if !ok {
		return
	}
	q := r.URL.Query()
	base, head := strings.TrimSpace(q.Get("base")), strings.TrimSpace(q.Get("head"))
	rc.v.Title = "Compare"
	data := compareData{View: rc.v, Repo: rc.rv, Base: base, Head: head, Single: q.Get("path"), Stale: rc.stale}
	if base == "" || head == "" {
		s.render(w, r, http.StatusOK, "compare", data)
		return
	}

	text, meta, err := s.api.Compare(r.Context(), rc.tok, rc.repo.ID, base, head, data.Single)
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	budget := diff.DefaultBudget
	if data.Single != "" {
		budget.Force = data.Single
	}
	data.Ready = true
	data.Patch = diff.Parse(text, budget)
	data.Patch.Truncated = meta.Truncated
	data.Truncated = meta.Truncated
	selfURL := rc.rv.URL() + "/compare?base=" + url.QueryEscape(base) + "&head=" + url.QueryEscape(head)
	data.Files = fileViews(rc.rv, data.Patch.Files, selfURL, head)
	data.Stale = firstNonEmpty(rc.stale, staleSentence(meta))
	s.render(w, r, http.StatusOK, "compare", data)
}
