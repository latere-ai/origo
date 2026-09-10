// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/wal"
)

// TestSSHPathForms is decision 8: every form of the table is accepted
// and names the same repository, and everything else is refused.
func TestSSHPathForms(t *testing.T) {
	const id = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	name := auth.RepoRef{Owner: "acme", Slug: "app"}
	for _, c := range []struct {
		in   string
		want auth.RepoRef
	}{
		{"/acme/app.git", name},
		{"acme/app.git", name},
		{"/acme/app", name},
		{"acme/app", name},
		{"/r/" + id + ".git", auth.RepoRef{ID: id}},
		{"r/" + id + ".git", auth.RepoRef{ID: id}},
		{"/r/" + id, auth.RepoRef{ID: id}},
		{"r/" + id, auth.RepoRef{ID: id}},
	} {
		got, err := ParsePath(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParsePath(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}

	for _, in := range []string{
		"",
		"~acme/app.git",
		"/~acme/app.git",
		"acme",
		"/acme",
		"acme/app/deep.git",
		"/acme//app.git",
		"//acme/app.git",
		"/r/not-a-uuid.git",
		"/r/0F5C1D2E-3A4B-4C5D-8E6F-7A8B9C0D1E2F.git",
		"/v1/repos.git",
		"/acme/../app.git",
		"/acme/.git",
		"/-acme/app.git",
		"/acme/app\x00.git",
		"/acme/app\n.git",
		"/" + strings.Repeat("a", 200) + "/app.git",
		strings.Repeat("a", MaxPathBytes+1),
	} {
		if got, err := ParsePath(in); err == nil {
			t.Errorf("ParsePath(%q) = %+v, want a refusal", in, got)
		}
	}
}

// TestSSHCommandForms holds the quoting git emits and the surface of two
// entries: the argument is parsed here and never by a shell.
func TestSSHCommandForms(t *testing.T) {
	for _, c := range []struct {
		in      string
		service string
		arg     string
	}{
		{"git-upload-pack '/acme/app.git'", ServiceUploadPack, "/acme/app.git"},
		{"git-receive-pack '/acme/app.git'", ServiceReceivePack, "/acme/app.git"},
		{"git-upload-pack /acme/app.git", ServiceUploadPack, "/acme/app.git"},
		{`git-upload-pack '/acme/a'\''b.git'`, ServiceUploadPack, "/acme/a'b.git"},
		{"git-upload-pack  '/acme/app.git' ", ServiceUploadPack, "/acme/app.git"},
	} {
		service, arg, err := ParseCommand(c.in)
		if err != nil || service != c.service || arg != c.arg {
			t.Errorf("ParseCommand(%q) = %q %q, %v", c.in, service, arg, err)
		}
	}
	for _, in := range []string{
		"",
		"git-upload-pack",
		"ls '/acme/app.git'",
		"git-upload-archive '/acme/app.git'",
		"git-lfs-authenticate '/acme/app.git' download",
		"git upload-pack '/acme/app.git'",
		"git-upload-pack '/acme/app.git",
		"git-upload-pack '/acme/app.git' ; ls",
		"git-upload-pack /acme/app.git; ls",
		"git-upload-pack $(ls)",
		"git-upload-pack ''",
	} {
		if service, arg, err := ParseCommand(in); err == nil {
			t.Errorf("ParseCommand(%q) = %q %q, want a refusal", in, service, arg)
		}
	}
}

// FuzzSSHPath holds the parser to spec 003's grammar: nothing it accepts
// is a name or an id the log would refuse, and nothing it accepts names
// a reserved segment. The seed corpus runs in the test gate on every
// push; the weekly fuzz job runs it for 40 seconds.
func FuzzSSHPath(f *testing.F) {
	for _, seed := range []string{
		"/acme/app.git", "acme/app", "/r/0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f.git",
		"~acme/app.git", "/v1/repos", "/r/r.git", "//", "/acme/app/../..",
		"/acme/app\x00", "/.git/config", "/acme/" + strings.Repeat("a", 130),
		"/-/-", "/A/B.git", "/r/0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2fx",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		ref, err := ParsePath(in)
		if err != nil {
			return
		}
		switch {
		case ref.ID != "":
			if !wal.ValidID(ref.ID) {
				t.Fatalf("ParsePath(%q) accepted the id %q, which the log refuses", in, ref.ID)
			}
			if ref.Owner != "" || ref.Slug != "" {
				t.Fatalf("ParsePath(%q) = %+v: the id form carries a name", in, ref)
			}
		default:
			if !wal.ValidLabel(ref.Owner) || !wal.ValidLabel(ref.Slug) {
				t.Fatalf("ParsePath(%q) accepted %q/%q, which the log refuses", in, ref.Owner, ref.Slug)
			}
			if ReservedOwners[ref.Owner] {
				t.Fatalf("ParsePath(%q) accepted the reserved owner %q", in, ref.Owner)
			}
		}
	})
}
