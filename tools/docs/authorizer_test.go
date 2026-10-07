// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheAuthorizerPageStatesTheCursorRule holds the directory section of
// docs/authorizer.md to the cursor an endpoint writes: Origo encrypts it
// before a caller sees it, returns it only to the endpoint that wrote it
// on a request from the subject it was written for, and bounds it at 512
// bytes. The page therefore no longer asks an endpoint to keep the cursor
// free of what a caller may not see.
func TestTheAuthorizerPageStatesTheCursorRule(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(root(t), "docs", "authorizer.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	start := strings.Index(page, "\n## The directory\n")
	if start < 0 {
		t.Fatal("docs/authorizer.md has no section named The directory")
	}
	section := page[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	text := strings.Join(strings.Fields(section), " ")
	for _, want := range []string{
		"Origo encrypts `next_cursor` before any caller sees it",
		"only to the endpoint that wrote it",
		"from the subject it was written for",
		"up to 512 bytes",
		"is no answer",
		"It is not an authorization",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the directory section does not say %q", want)
		}
	}
	for _, gone := range []string{"put nothing in it", "do not begin a `next_cursor`"} {
		if strings.Contains(text, gone) {
			t.Errorf("the directory section still says %q", gone)
		}
	}
}
