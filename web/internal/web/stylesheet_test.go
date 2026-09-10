// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"regexp"
	"strings"
	"testing"
)

var (
	tokenUse    = regexp.MustCompile(`var\((--[a-z0-9-]+)`)
	tokenDefine = regexp.MustCompile(`(?m)^\s*(--[a-z0-9-]+)\s*:`)
	lightBlock  = regexp.MustCompile(`(?s):root \{(.*?)\n\}`)
	darkBlock   = regexp.MustCompile(`(?s)@media \(prefers-color-scheme: dark\) \{\s*:root \{(.*?)\n  \}`)
)

// TestBothThemesAreComplete asserts the property a page cannot be eyeballed
// into having: every colour the interface uses is a token, every token it
// uses is defined, and every colour token has a value in both themes. A
// colour defined only in the light block is a page that goes unreadable in
// the dark one, and nothing but this catches it.
func TestBothThemesAreComplete(t *testing.T) {
	css := string(mustAsset(t, "app.css"))

	if strings.Count(css, "{") != strings.Count(css, "}") {
		t.Fatalf("the stylesheet has %d opening and %d closing braces",
			strings.Count(css, "{"), strings.Count(css, "}"))
	}

	defined := map[string]bool{}
	for _, m := range tokenDefine.FindAllStringSubmatch(css, -1) {
		defined[m[1]] = true
	}
	for _, m := range tokenUse.FindAllStringSubmatch(css, -1) {
		if !defined[m[1]] {
			t.Errorf("%s is used and never defined", m[1])
		}
	}

	light := lightBlock.FindStringSubmatch(css)
	dark := darkBlock.FindStringSubmatch(css)
	if light == nil || dark == nil {
		t.Fatal("the stylesheet has no light block or no dark block")
	}
	inDark := map[string]bool{}
	for _, m := range tokenDefine.FindAllStringSubmatch(dark[1], -1) {
		inDark[m[1]] = true
	}
	for _, m := range tokenDefine.FindAllStringSubmatch(light[1], -1) {
		name := m[1]
		if !isColour(name) || inDark[name] {
			continue
		}
		t.Errorf("%s has a light value and no dark one", name)
	}

	// The theme follows the system setting alone. A switch would need a
	// script or a cookie, and both are out.
	if strings.Contains(css, "data-theme") {
		t.Error("the stylesheet carries a theme switch")
	}
	if !strings.Contains(css, "color-scheme: light dark") {
		t.Error("the page does not tell the browser it has both themes")
	}
}

// isColour reports whether a token names a colour rather than a size, a
// radius, or a typeface.
func isColour(token string) bool {
	for _, part := range []string{"bg", "fg", "accent", "border", "diff", "focus"} {
		if strings.Contains(token, part) {
			return true
		}
	}
	return false
}

// TestTheNarrowRulesAreThere asserts the structural half of the narrow-width
// criterion: one breakpoint, data tables that restack, and code and diffs
// that scroll inside their own box rather than reflowing, because a wrapped
// line of source is a lie about the file.
func TestTheNarrowRulesAreThere(t *testing.T) {
	css := string(mustAsset(t, "app.css"))
	if n := strings.Count(css, "@media (max-width"); n != 1 {
		t.Errorf("the stylesheet has %d width breakpoints, want one", n)
	}
	for _, want := range []string{
		"table.stackable",  // data tables restack
		".scroller",        // wide content scrolls inside its own box
		"overflow-x: auto", //
		"--tap: 44px",      // tap targets
		"max-width: 100%",  // nothing forces the document wider than the screen
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the stylesheet has no %q", want)
		}
	}

	// Every wide table on every screen is inside a scrolling container, so
	// the document itself never scrolls sideways.
	h := newHarness(t)
	c := h.signedIn("alice")
	for name, path := range h.screens() {
		page := doc(t, h.get(path, c).Body.String())
		for _, table := range elements(page, "table") {
			var boxed bool
			for p := table.Parent; p != nil; p = p.Parent {
				if strings.Contains(attr(p, "class"), "scroller") ||
					strings.Contains(attr(p, "class"), "diff-body") {
					boxed = true
					break
				}
			}
			if !boxed {
				t.Errorf("%s: a table that can push the document sideways", name)
			}
		}
	}
}
