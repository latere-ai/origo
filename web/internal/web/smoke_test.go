// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestEveryScreenRenders(t *testing.T) {
	h := newHarness(t)
	c := h.signedIn("alice")
	for _, path := range []string{
		"/",
		h.repoPath(""),
		h.repoPath("/refs"),
		h.repoPath("/log"),
		h.repoPath("/commit/9f3c1abf20d4e7c8b5a1930fe6d2c4471be08a3d"),
		h.repoPath("/compare"),
		h.repoPath("/compare?base=main&head=next"),
		h.repoPath("/tree/"),
		h.repoPath("/tree/internal"),
		h.repoPath("/blob/README.md"),
		h.repoPath("/blob/logo.png"),
		h.repoPath("/blob/huge.json"),
		"/tokens",
		"/docs/agents",
	} {
		rec := h.get(path, c)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d\n%s", path, rec.Code, rec.Body.String())
			continue
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<!DOCTYPE html>") {
			t.Errorf("%s: not a document", path)
		}
		if strings.Contains(body, "<no value>") {
			t.Errorf("%s: a template hole was rendered empty", path)
		}
	}
	if rec := h.get("/sign-in"); rec.Code != http.StatusOK {
		t.Errorf("/sign-in signed out: status %d", rec.Code)
	}
	if rec := h.get("/sign-in", c); rec.Code != http.StatusFound {
		t.Errorf("/sign-in signed in: want a redirect to the page asked for, got %d", rec.Code)
	}
}
