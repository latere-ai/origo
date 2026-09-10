// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit"

	"github.com/latere-ai/origo-web/internal/origo"
)

// view is what every screen shares: its title, which section of a repository
// it is, and what the masthead offers.
type view struct {
	Title       string
	Section     string
	SignedIn    bool
	KeysEnabled bool
	CSRF        string
}

// CSRFField is the form field the two POST routes read their token from.
func (v view) CSRFField() string { return authkit.CSRFFieldName() }

// repoView is the repository every repository screen is about.
type repoView struct {
	ID            string
	Owner         string
	Slug          string
	DefaultBranch string
	Ref           string
	Size          string
	Pushed        string
	PushedExact   string
	Frozen        bool
	Empty         bool
}

// URL is where this repository lives in this interface.
//
// Every repository is addressed by its id. Origo's JSON surface resolves no
// owner and slug (spec 023), so an owner-and-slug URL could not be served,
// and an id is what survives a rename in any case. The name is shown; the
// address is the id.
func (r repoView) URL() string { return "/r/" + url.PathEscape(r.ID) }

// RefQuery carries the current reference to the next screen, and is empty on
// the default branch so an ordinary URL stays short.
func (r repoView) RefQuery() string {
	if r.Ref == "" || r.Ref == r.DefaultBranch {
		return ""
	}
	return "?ref=" + url.QueryEscape(r.Ref)
}

func newRepoView(r origo.Repo, ref string) *repoView {
	v := &repoView{
		ID:            r.ID,
		Owner:         r.Owner,
		Slug:          r.Slug,
		DefaultBranch: r.DefaultBranch,
		Ref:           ref,
		Size:          humanSize(r.SizeBytes),
		Frozen:        r.FrozenAt != nil,
		Empty:         r.Head == "",
	}
	if v.Ref == "" {
		v.Ref = r.DefaultBranch
	}
	if r.PushedAt != nil && !r.PushedAt.IsZero() {
		v.Pushed = relTime(*r.PushedAt)
		v.PushedExact = absTime(*r.PushedAt)
	}
	return v
}

// refSelector is the branch and tag control. It is a plain GET form: a
// selection is a submission, and it lands on the same screen at the new
// revision.
type refSelector struct {
	Action   string
	AllRefs  string
	Ref      string
	Branches []origo.Ref
	Tags     []origo.Ref
}

// Empty reports that there is nothing to choose between.
func (s refSelector) Empty() bool { return len(s.Branches) == 0 && len(s.Tags) == 0 }

// entryView is one row of a tree.
type entryView struct {
	Name string
	Type string
	Size string
	URL  string
}

// commitView is one row of a log, and the header of a commit screen.
type commitView struct {
	SHA       string
	Short     string
	Subject   string
	Body      string
	Author    string
	Email     string
	When      string
	WhenExact string
	URL       string
}

func newCommitView(c origo.Commit, repoURL string) commitView {
	return commitView{
		SHA:       c.SHA,
		Short:     origo.Short(c.SHA),
		Subject:   c.Subject(),
		Body:      c.Body(),
		Author:    c.Author.Name,
		Email:     c.Author.Email,
		When:      relTime(c.Author.At),
		WhenExact: absTime(c.Author.At),
		URL:       repoURL + "/commit/" + url.PathEscape(c.SHA),
	}
}

// humanSize writes a byte count the way a person reads one.
func humanSize(n int64) string {
	switch {
	case n < 0:
		return ""
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n) / 1024
	for _, u := range units {
		if v < 1024 {
			if v < 10 {
				return fmt.Sprintf("%.1f %s", v, u)
			}
			return fmt.Sprintf("%.0f %s", v, u)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.0f EB", v)
}

// now is the clock, replaced in a test so a rendered page is comparable.
var now = time.Now

// relTime says how long ago something happened, in the register a person
// reads. Past about a year it gives the date instead, because "14 months
// ago" is less useful than the month.
func relTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now().Sub(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/(24*30)), "month") + " ago"
	}
	return t.Format("2 Jan 2006")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// absTime is the exact time, kept beside every relative one so a person can
// read the fact and not only the impression.
func absTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2 Jan 2006, 15:04 -0700")
}

// staleSentence is the line a page carries when Origo served it without a
// currency check. A machine consumer ignores that header; a person reading a
// commit log must not.
func staleSentence(m origo.Meta) string {
	if !m.StaleSet {
		return ""
	}
	secs := int(m.Stale.Seconds())
	switch {
	case secs < 60:
		return plural(secs, "second")
	case secs < 3600:
		return plural(secs/60, "minute")
	}
	return plural(secs/3600, "hour")
}

// cleanPath normalises a path out of a URL: no leading or trailing slash, no
// . or .. segment, and no empty segment.
func cleanPath(p string) string {
	var out []string
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// parentPath is the directory holding p, empty at the root.
func parentPath(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// crumb is one step of a path breadcrumb.
type crumb struct {
	Name string
	URL  string
}

// pathCrumbs turns a path into links, each pointing at the tree that holds
// it. The last segment is the page itself and carries no link.
func pathCrumbs(repoURL, ref, path string) []crumb {
	if path == "" {
		return nil
	}
	q := ""
	if ref != "" {
		q = "?ref=" + url.QueryEscape(ref)
	}
	segs := strings.Split(path, "/")
	out := make([]crumb, 0, len(segs))
	for i, s := range segs {
		c := crumb{Name: s}
		if i < len(segs)-1 {
			c.URL = repoURL + "/tree/" + escapePath(strings.Join(segs[:i+1], "/")) + q
		}
		out = append(out, c)
	}
	return out
}

// escapePath escapes each segment of a path, keeping the separators.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// RefQueryValue is the reference a link should carry, empty on the default
// branch.
func (r repoView) RefQueryValue() string {
	if r.Ref == "" || r.Ref == r.DefaultBranch {
		return ""
	}
	return r.Ref
}
