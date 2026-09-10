// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"bytes"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"latere.ai/x/pkg/md"

	"github.com/latere-ai/origo-web/internal/origo"
)

// ---- tree ----

type treeData struct {
	View    view
	Repo    *repoView
	Path    string
	Crumbs  []crumb
	Parent  string
	Entries []entryView
	NextURL string
	History string
	Stale   string
	Empty   bool
}

// handleTree lists one directory at one revision.
//
// One call, never recursive: a repository with a hundred thousand files
// costs one page of five thousand entries. The last commit that touched each
// entry is deliberately absent, because that is one call per row and Origo
// offers no batch for it; the directory's history is one link away instead.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "tree")
	if !ok {
		return
	}
	path := cleanPath(r.PathValue("path"))
	page, err := s.api.Tree(r.Context(), rc.tok, rc.repo.ID, rc.ref, origo.TreeOptions{
		Path: path, Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return
	}

	data := treeData{
		View:    rc.v,
		Repo:    rc.rv,
		Path:    path,
		Crumbs:  pathCrumbs(rc.rv.URL(), rc.rv.RefQueryValue(), path),
		Entries: s.entryViews(rc.rv, page.Items),
		Stale:   firstNonEmpty(rc.stale, staleSentence(page.Meta)),
		Empty:   len(page.Items) == 0,
		History: rc.rv.URL() + "/log?" + logQuery(rc.rv, path),
	}
	if path != "" {
		data.Parent = rc.rv.URL() + "/tree/" + escapePath(parentPath(path)) + rc.rv.RefQuery()
	}
	if page.Next != "" {
		q := url.Values{}
		setIfNotEmpty(q, "ref", rc.rv.RefQueryValue())
		q.Set("cursor", page.Next)
		data.NextURL = rc.rv.URL() + "/tree/" + escapePath(path) + "?" + q.Encode()
	}
	rc.v.Title = firstNonEmpty(path, rc.repo.Slug) + " at " + rc.ref
	data.View = rc.v
	s.render(w, r, http.StatusOK, "tree", data)
}

func logQuery(rv *repoView, path string) string {
	q := url.Values{}
	setIfNotEmpty(q, "ref", rv.RefQueryValue())
	setIfNotEmpty(q, "path", path)
	return q.Encode()
}

func (s *Server) entryViews(rv *repoView, entries []origo.Entry) []entryView {
	out := make([]entryView, 0, len(entries))
	q := rv.RefQuery()
	for _, e := range entries {
		v := entryView{Name: e.Name(), Type: entryType(e), Size: humanSize(e.Size)}
		if e.IsDir() {
			v.Name += "/"
			v.Size = ""
			v.URL = rv.URL() + "/tree/" + escapePath(e.Path) + q
		} else {
			v.URL = rv.URL() + "/blob/" + escapePath(e.Path) + q
		}
		out = append(out, v)
	}
	return out
}

// entryType names what a tree entry is, in words a person reads: a symbolic
// link and an executable are the two a listing must distinguish.
func entryType(e origo.Entry) string {
	switch {
	case e.Type == "tree":
		return "directory"
	case e.Type == "commit":
		return "submodule"
	case e.Mode == "120000":
		return "symlink"
	case e.Mode == "100755":
		return "executable"
	default:
		return "file"
	}
}

// ---- file ----

type blobData struct {
	View       view
	Repo       *repoView
	Path       string
	Crumbs     []crumb
	Name       string
	Size       string
	Type       string
	Mode       string
	Lines      []sourceLine
	LineCount  int
	Truncated  bool
	TooLarge   bool
	Binary     bool
	RawURL     string
	HistoryURL string
	CloneHTTPS string
	Stale      string
}

type sourceLine struct {
	N    int
	Text string
}

// renderLimit is how many bytes of a file this interface renders. Past it
// the screen shows the first part and says so: the cap is enforced by the
// request, with a Range, so the rest is never sent.
const renderLimit = 1 << 20

// fetchLimit is where the interface stops asking at all. It is Origo's own
// blob cap (spec 009), above which a read without a Range is refused, so a
// file this large is a download and a clone and not a page.
const fetchLimit = 50 << 20

// handleBlob shows one file.
//
// The tree call comes first and is what makes the size known before any byte
// is fetched: spec 009's blob route takes a sha and no path, so the entry has
// to be found anyway, and finding it also decides whether to fetch at all.
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "tree")
	if !ok {
		return
	}
	path := cleanPath(r.PathValue("path"))
	entry, ok := s.findEntry(w, r, rc, path)
	if !ok {
		return
	}

	data := blobData{
		View:       rc.v,
		Repo:       rc.rv,
		Path:       path,
		Crumbs:     pathCrumbs(rc.rv.URL(), rc.rv.RefQueryValue(), path),
		Name:       entry.Name(),
		Size:       humanSize(entry.Size),
		Mode:       entry.Mode,
		Type:       entryType(entry),
		RawURL:     rc.rv.URL() + "/raw/" + escapePath(path) + rc.rv.RefQuery(),
		HistoryURL: rc.rv.URL() + "/log?" + logQuery(rc.rv, path),
		CloneHTTPS: s.cfg.CloneHTTPS(rc.repo.Owner, rc.repo.Slug),
		Stale:      rc.stale,
	}
	rc.v.Title = entry.Name()
	data.View = rc.v

	if entry.Size > fetchLimit {
		data.TooLarge = true
		s.render(w, r, http.StatusOK, "blob", data)
		return
	}

	// The cap is enforced by the request. Origo honours Range, so the
	// bytes past it are never sent, never read, and never discarded.
	blob, err := s.api.BlobRange(r.Context(), rc.tok, rc.repo.ID, entry.SHA, renderLimit)
	if err != nil {
		if origo.TooLarge(err) {
			data.TooLarge = true
			s.render(w, r, http.StatusOK, "blob", data)
			return
		}
		s.readFailed(w, r, rc.req, err)
		return
	}
	data.Type = describeType(blob.ContentType, data.Type)

	// A file is rendered as text only when the type Origo detected is
	// textual and the bytes decode as UTF-8. Everything else is a
	// download: name, size, mode and type, and no bytes.
	if !textual(blob.ContentType) || !utf8.Valid(blob.Body) {
		data.Binary = true
		s.render(w, r, http.StatusOK, "blob", data)
		return
	}
	data.Truncated = blob.Partial && int64(len(blob.Body)) < entry.Size
	data.Lines = sourceLines(string(blob.Body))
	data.LineCount = len(data.Lines)
	s.render(w, r, http.StatusOK, "blob", data)
}

// findEntry reads one directory to find one file's object id and size.
func (s *Server) findEntry(w http.ResponseWriter, r *http.Request, rc repoContext, path string) (origo.Entry, bool) {
	if path == "" {
		s.notFound(w, r, rc.v)
		return origo.Entry{}, false
	}
	page, err := s.api.Tree(r.Context(), rc.tok, rc.repo.ID, rc.ref, origo.TreeOptions{Path: parentPath(path)})
	if err != nil {
		s.readFailed(w, r, rc.req, err)
		return origo.Entry{}, false
	}
	for _, e := range page.Items {
		if e.Path == path && !e.IsDir() {
			return e, true
		}
	}
	s.notFound(w, r, rc.v)
	return origo.Entry{}, false
}

// maxLineRunes bounds one rendered line. A minified file is one line of two
// megabytes, and a browser asked to lay that out stops being a browser.
const maxLineRunes = 4000

func sourceLines(text string) []sourceLine {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	parts := strings.Split(text, "\n")
	out := make([]sourceLine, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSuffix(p, "\r")
		if utf8.RuneCountInString(p) > maxLineRunes {
			p = string([]rune(p)[:maxLineRunes]) + " …"
		}
		out = append(out, sourceLine{N: i + 1, Text: p})
	}
	return out
}

// textual reports whether the type Origo detected is one this interface
// renders as text.
func textual(ct string) bool {
	ct, _, _ = strings.Cut(ct, ";")
	ct = strings.TrimSpace(strings.ToLower(ct))
	switch {
	case ct == "":
		return false
	case strings.HasPrefix(ct, "text/"):
		return true
	case strings.HasSuffix(ct, "+json"), strings.HasSuffix(ct, "+xml"):
		return true
	}
	switch ct {
	case "application/json", "application/xml", "application/javascript",
		"application/x-sh", "application/x-yaml", "application/yaml":
		return true
	}
	return false
}

// describeType names the content type in the register of a person, falling
// back to what the tree said the entry is.
func describeType(ct, fallback string) string {
	ct, _, _ = strings.Cut(ct, ";")
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return fallback
	}
	return ct
}

// ---- raw ----

// handleRaw copies the bytes through, as an attachment and with the type
// Origo detected. The body is streamed and never buffered, so a large file
// costs this service no memory.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	rc, ok := s.openRepo(w, r, "tree")
	if !ok {
		return
	}
	path := cleanPath(r.PathValue("path"))
	entry, ok := s.findEntry(w, r, rc, path)
	if !ok {
		return
	}
	resp, err := s.api.BlobStream(r.Context(), rc.tok, rc.repo.ID, entry.SHA)
	if err != nil {
		if origo.TooLarge(err) {
			rc.v.Title = "Too large"
			s.render(w, r, http.StatusOK, "message", messageData{
				View:    rc.v,
				Repo:    rc.rv,
				Heading: "Too large to serve",
				Body:    "This file is larger than the installation will send in one response. Clone the repository to read it.",
				Links:   []crumb{{Name: "Back to the file", URL: rc.rv.URL() + "/blob/" + escapePath(path) + rc.rv.RefQuery()}},
			})
			return
		}
		s.readFailed(w, r, rc.req, err)
		return
	}
	defer resp.Body.Close()

	h := w.Header()
	h.Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "application/octet-stream"))
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		h.Set("Content-Length", cl)
	}
	// An attachment, always. A repository holds bytes nobody vetted, and a
	// browser must never be invited to run them on this origin.
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(entry.Name()))
	_, _ = io.Copy(w, resp.Body)
}

// renderReadme turns a readme into HTML, or into text when it is not
// Markdown.
//
// The Markdown renderer is goldmark through latere.ai/x/pkg/md, configured
// without its unsafe option, so raw HTML inside a repository's readme is
// dropped rather than rendered. The page's Content-Security-Policy is the
// second line of the same defence.
func renderReadme(name string, body []byte) (html template.HTML, text string) {
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".md") && !strings.HasSuffix(lower, ".markdown") {
		return "", string(body)
	}
	out, err := md.Render(body)
	if err != nil {
		return "", string(body)
	}
	// goldmark runs without its unsafe option here, so raw HTML in the
	// source is dropped rather than rendered and what remains is the
	// renderer's own markup.
	return template.HTML(demoteHeadings(out)), ""
}

// demoteHeadings pushes every heading of a rendered readme down one level,
// so the page keeps exactly one h1 of its own and the readme's own headings
// nest under it. A document with two h1 elements has no outline, and a screen
// reader reads the outline.
func demoteHeadings(fragment []byte) []byte {
	nodes, err := html.ParseFragment(bytes.NewReader(fragment), &html.Node{
		Type: html.ElementNode, Data: "div", DataAtom: atom.Div,
	})
	if err != nil {
		return fragment
	}
	demote := map[string]string{"h1": "h2", "h2": "h3", "h3": "h4", "h4": "h5", "h5": "h6"}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if to, ok := demote[n.Data]; ok {
				n.Data, n.DataAtom = to, atom.Lookup([]byte(to))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	var out bytes.Buffer
	for _, n := range nodes {
		walk(n)
		if err := html.Render(&out, n); err != nil {
			return fragment
		}
	}
	return out.Bytes()
}
