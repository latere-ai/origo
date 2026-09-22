// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"testing"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"latere.ai/x/origo/internal/auth"
	"latere.ai/x/origo/internal/config"
	"latere.ai/x/origo/internal/metrics"
	"latere.ai/x/origo/internal/wal"
)

func ownerPolicyLog(t *testing.T) *wal.Log {
	t.Helper()
	set := metrics.Register(pkgmetrics.NewRegistry())
	return wal.New(wal.Options{Store: wal.NewMemStore(), Prefix: config.Prefix, Metrics: set, Logger: slog.New(slog.DiscardHandler)})
}

// TestWalObjectsBacksTheOwnerPolicy covers the owner policy's view of the
// node's own repositories (Origo spec 028): a repository is looked up by
// id or by owner/slug, its creator is reported, an unknown ref is
// Object{Exists:false}, and the directory lists the subject's own,
// paginated.
func TestWalObjectsBacksTheOwnerPolicy(t *testing.T) {
	const (
		alice = "https://iss|alice"
		bob   = "https://iss|bob"
		one   = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
		two   = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
		three = "2b3c4d5e-6f70-4a8b-9c0d-1e2f3a4b5c6d"
	)
	log := ownerPolicyLog(t)
	ctx := context.Background()
	for _, r := range []struct{ id, slug, creator string }{
		{one, "app", alice},
		{two, "lib", alice},
		{three, "tool", bob},
	} {
		if _, err := log.CreateRepo(ctx, wal.Meta{ID: r.id, Owner: "acme", Slug: r.slug, Creator: r.creator}, "main"); err != nil {
			t.Fatal(err)
		}
	}
	w := walObjects{log: log}

	// By id: exists, with the creator.
	if obj, err := w.Object(ctx, auth.RepoRef{ID: one}); err != nil || !obj.Exists || obj.Owner != alice {
		t.Fatalf("by id: %+v, %v", obj, err)
	}
	// By owner/slug: resolves to the id, and reports the creator.
	if obj, err := w.Object(ctx, auth.RepoRef{Owner: "acme", Slug: "tool"}); err != nil || !obj.Exists || obj.Owner != bob {
		t.Fatalf("by name: %+v, %v", obj, err)
	}
	// An unknown id and an unresolved name are Object{Exists:false}, not
	// an error, so the policy reads them as not owned.
	for _, ref := range []auth.RepoRef{{ID: "3c4d5e6f-7081-4a9b-8c0d-1e2f3a4b5c6d"}, {Owner: "acme", Slug: "missing"}, {}} {
		if obj, err := w.Object(ctx, ref); err != nil || obj.Exists {
			t.Fatalf("unknown %+v: %+v, %v", ref, obj, err)
		}
	}

	// The directory lists a subject's own, in id order, paginated.
	dir, err := w.Owned(ctx, alice, "", 50)
	if err != nil || len(dir.Repos) != 2 || dir.Repos[0].ID != one || dir.Repos[1].ID != two {
		t.Fatalf("alice's directory: %+v, %v", dir, err)
	}
	first, err := w.Owned(ctx, alice, "", 1)
	if err != nil || len(first.Repos) != 1 || first.Repos[0].ID != one || first.NextCursor != one {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	second, err := w.Owned(ctx, alice, first.NextCursor, 1)
	if err != nil || len(second.Repos) != 1 || second.Repos[0].ID != two || second.NextCursor != "" {
		t.Fatalf("second page: %+v, %v", second, err)
	}
	// A subject that created nothing lists nothing.
	if dir, err := w.Owned(ctx, "https://iss|carol", "", 50); err != nil || len(dir.Repos) != 0 {
		t.Fatalf("carol's directory: %+v, %v", dir, err)
	}
}

// TestOwnerPolicyNodeMode is the node in owner-policy mode: with no
// authorizer configured, newNode builds the built-in policy and a subject
// reaches a repository it created and nothing it did not.
func TestOwnerPolicyNodeMode(t *testing.T) {
	log := ownerPolicyLog(t)
	admins := []string{"https://iss|root"}
	p := auth.NewOwnerPolicy(admins, walObjects{log: log})
	ctx := context.Background()
	const (
		alice = "https://iss|alice"
		id    = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	)
	if _, err := log.CreateRepo(ctx, wal.Meta{ID: id, Owner: "acme", Slug: "app", Creator: alice}, "main"); err != nil {
		t.Fatal(err)
	}
	g := auth.NewGuard(p, slog.New(slog.DiscardHandler))
	// The creator reads and writes; a stranger is refused.
	if err := g.Authorize(ctx, auth.Principal{Subject: alice}, auth.RepoRef{ID: id}, auth.ActionWrite); err != nil {
		t.Fatalf("the creator was refused a write: %v", err)
	}
	if err := g.Authorize(ctx, auth.Principal{Subject: "https://iss|mallory"}, auth.RepoRef{ID: id}, auth.ActionRead); err == nil {
		t.Fatal("a stranger read a repository it did not create")
	}
	// An admin reaches it, and the directory lists the creator's own.
	if err := g.Authorize(ctx, auth.Principal{Subject: "https://iss|root"}, auth.RepoRef{ID: id}, auth.ActionAdmin); err != nil {
		t.Fatalf("an admin was refused: %v", err)
	}
	if dir, err := g.Directory(ctx, auth.Principal{Subject: alice}, "", 50); err != nil || len(dir.Repos) != 1 || dir.Repos[0].ID != id {
		t.Fatalf("the creator's directory: %+v, %v", dir, err)
	}
}
