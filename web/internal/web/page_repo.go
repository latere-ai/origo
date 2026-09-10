// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/latere-ai/origo-web/internal/origo"
)

// repoContext is what every repository screen resolves first: the
// repository, the reference being read, and the frame around them.
type repoContext struct {
	req
	repo  origo.Repo
	rv    *repoView
	ref   string
	stale string
}

// openRepo resolves the repository named in the path.
//
// The read is issued with the reader's token when there is one and with no
// Authorization header when there is not, and the answer is rendered as it
// arrives. There is no branch on "is there a session" here: on an
// installation that later answers a tokenless read, the same code path shows
// the repository.
func (s *Server) openRepo(w http.ResponseWriter, r *http.Request, section string) (repoContext, bool) {
	rq := s.begin(w, r, section)
	id := r.PathValue("id")

	repo, meta, err := s.api.Repo(r.Context(), rq.tok, id)
	if err != nil {
		s.readFailed(w, r, rq, err)
		return repoContext{}, false
	}

	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = repo.DefaultBranch
	}
	rv := newRepoView(repo, ref)
	rq.v.Title = repo.Owner + "/" + repo.Slug
	s.sessions.Remember(w, r, repo.ID)
	return repoContext{req: rq, repo: repo, rv: rv, ref: ref, stale: staleSentence(meta)}, true
}

// readFailed turns a refusal from Origo into the one screen it deserves.
func (s *Server) readFailed(w http.ResponseWriter, r *http.Request, rq req, err error) {
	switch {
	case origo.Unauthenticated(err):
		// The token was refused. Clear the session and send the person
		// to sign in once; a repeat is an error page, not a loop.
		s.sessions.Clear(w)
		s.signIn(w, r, rq.v, true)
	case origo.Absent(err):
		s.notFound(w, r, rq.v)
	case origo.Unavailable(err):
		s.unavailable(w, r, rq.v)
	default:
		s.broken(w, r, rq.v, err)
	}
}

// refsFor reads the branches and the tags. Two calls, whatever the
// repository holds, because spec 009 pages references by prefix and not by
// row.
func (s *Server) refsFor(r *http.Request, tok, id string) (branches, tags []origo.Ref, truncated bool, err error) {
	branches, bm, err := s.api.Refs(r.Context(), tok, id, "refs/heads/")
	if err != nil {
		return nil, nil, false, err
	}
	tags, tm, err := s.api.Refs(r.Context(), tok, id, "refs/tags/")
	if err != nil {
		return nil, nil, false, err
	}
	sortRefs(branches)
	sortRefs(tags)
	return branches, tags, bm.Truncated || tm.Truncated, nil
}

func sortRefs(rs []origo.Ref) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Short() < rs[j].Short() })
}

// ---- overview ----

type overviewData struct {
	View       view
	Repo       *repoView
	Selector   refSelector
	Entries    []entryView
	TreeCursor string
	Last       *commitView
	Readme     *readmeView
	CloneHTTPS string
	CloneSSH   string
	ArchiveURL string
	Branches   int
	Tags       int
	Stale      string
	Empty      bool
}

type readmeView struct {
	Name string
	HTML template.HTML
	Text string
	URL  string
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "overview")
	if !ok {
		return
	}
	data := overviewData{
		View:       rc.v,
		Repo:       rc.rv,
		CloneHTTPS: s.cfg.CloneHTTPS(rc.repo.Owner, rc.repo.Slug),
		CloneSSH:   s.cfg.CloneSSH(rc.repo.Owner, rc.repo.Slug),
		Stale:      rc.stale,
	}

	branches, tags, _, err := s.refsFor(r, rc.tok, rc.repo.ID)
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	data.Branches, data.Tags = len(branches), len(tags)
	data.Selector = refSelector{Action: rc.rv.URL(), Ref: rc.ref, Branches: branches, Tags: tags}

	// A repository with no commit is a state, not a failure. Origo answers
	// the log of an empty repository with an empty page, and its tree with
	// a missing reference, so the screen says what to do next instead.
	if rc.repo.Head == "" && len(branches) == 0 {
		data.Empty = true
		s.render(w, r, http.StatusOK, "overview", data)
		return
	}
	data.ArchiveURL = s.cfg.ArchiveURL(rc.repo.ID, rc.ref)

	tree, err := s.api.Tree(r.Context(), rc.tok, rc.repo.ID, rc.ref, origo.TreeOptions{})
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	data.TreeCursor = tree.Next
	data.Entries = s.entryViews(rc.rv, tree.Items)
	if data.Stale == "" {
		data.Stale = staleSentence(tree.Meta)
	}

	log, err := s.api.Commits(r.Context(), rc.tok, rc.repo.ID, origo.CommitsOptions{Ref: rc.ref, Limit: 1})
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	if len(log.Items) > 0 {
		c := newCommitView(log.Items[0], rc.rv.URL())
		data.Last = &c
	}

	if entry, found := findReadme(tree.Items); found {
		data.Readme = s.readme(r, rc, entry)
	}
	s.render(w, r, http.StatusOK, "overview", data)
}

// findReadme picks the root readme, preferring the Markdown form. The tree
// page is already in hand, so this costs no call.
func findReadme(entries []origo.Entry) (origo.Entry, bool) {
	var plain origo.Entry
	var havePlain bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(strings.ToLower(e.Name()), "readme") {
			continue
		}
		lower := strings.ToLower(e.Name())
		if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
			return e, true
		}
		if !havePlain {
			plain, havePlain = e, true
		}
	}
	return plain, havePlain
}

// readmeLimit is how much of a readme this service will render. A readme
// larger than this is a file, and the file screen is where a file belongs.
const readmeLimit = 512 << 10

func (s *Server) readme(r *http.Request, rc repoContext, e origo.Entry) *readmeView {
	v := &readmeView{
		Name: e.Name(),
		URL:  rc.rv.URL() + "/blob/" + escapePath(e.Path) + rc.rv.RefQuery(),
	}
	if e.Size > readmeLimit {
		return v
	}
	blob, err := s.api.BlobRange(r.Context(), rc.tok, rc.repo.ID, e.SHA, readmeLimit)
	if err != nil {
		return v
	}
	v.HTML, v.Text = renderReadme(e.Name(), blob.Body)
	return v
}

// ---- references ----

type refsData struct {
	View      view
	Repo      *repoView
	Branches  []refRow
	Tags      []refRow
	Truncated bool
	Stale     string
}

type refRow struct {
	Name   string
	Short  string
	Target string
	Sha    string
	Tree   string
	Log    string
	Commit string
}

func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "refs")
	if !ok {
		return
	}
	branches, tags, truncated, err := s.refsFor(r, rc.tok, rc.repo.ID)
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}
	rc.v.Title = rc.repo.Owner + "/" + rc.repo.Slug + " references"
	s.render(w, r, http.StatusOK, "refs", refsData{
		View:      rc.v,
		Repo:      rc.rv,
		Branches:  refRows(rc.rv, branches),
		Tags:      refRows(rc.rv, tags),
		Truncated: truncated,
		Stale:     rc.stale,
	})
}

func refRows(rv *repoView, rs []origo.Ref) []refRow {
	out := make([]refRow, 0, len(rs))
	for _, ref := range rs {
		q := "?ref=" + url.QueryEscape(ref.Short())
		out = append(out, refRow{
			Name:   ref.Name,
			Short:  ref.Short(),
			Target: origo.Short(ref.Target()),
			Sha:    ref.Target(),
			Tree:   rv.URL() + "/tree/" + q,
			Log:    rv.URL() + "/log" + q,
			Commit: rv.URL() + "/commit/" + url.PathEscape(ref.Target()),
		})
	}
	return out
}

// ---- commit log ----

type logData struct {
	View    view
	Repo    *repoView
	Commits []commitView
	Path    string
	NextURL string
	Stale   string
	Empty   bool
}

// logPageSize is one screen of history. Origo's own default is the same.
const logPageSize = 50

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "log")
	if !ok {
		return
	}
	q := r.URL.Query()
	path := cleanPath(q.Get("path"))

	page, err := s.api.Commits(r.Context(), rc.tok, rc.repo.ID, origo.CommitsOptions{
		Ref: rc.ref, Path: path, Limit: logPageSize, Cursor: q.Get("cursor"),
	})
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}

	data := logData{
		View:  rc.v,
		Repo:  rc.rv,
		Path:  path,
		Stale: firstNonEmpty(rc.stale, staleSentence(page.Meta)),
		Empty: len(page.Items) == 0,
	}
	for _, c := range page.Items {
		data.Commits = append(data.Commits, newCommitView(c, rc.rv.URL()))
	}
	if page.Next != "" {
		next := url.Values{}
		setIfNotEmpty(next, "ref", rc.rv.RefQueryValue())
		setIfNotEmpty(next, "path", path)
		next.Set("cursor", page.Next)
		data.NextURL = rc.rv.URL() + "/log?" + next.Encode()
	}
	rc.v.Title = "Commits on " + rc.ref
	data.View = rc.v
	s.render(w, r, http.StatusOK, "log", data)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func setIfNotEmpty(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}
