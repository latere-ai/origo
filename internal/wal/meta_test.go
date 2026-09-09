// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const repoB = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"

func TestRepositoryMetadataAndNames(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	l := newTestLog(t, store)
	ix, err := l.CreateRepo(ctx, Meta{ID: repoA, Owner: "acme", Slug: "app"}, "trunk")
	if err != nil || ix.Refs["HEAD"] != "ref: refs/heads/trunk" {
		t.Fatalf("create: %+v, %v", ix, err)
	}
	if _, err := l.CreateRepo(ctx, Meta{ID: repoA, Owner: "acme", Slug: "other"}, "main"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate id: %v", err)
	}
	if _, err := l.CreateRepo(ctx, Meta{ID: repoB, Owner: "acme", Slug: "app"}, "main"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := l.ReadMeta(ctx, repoB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("meta of the refused create survived: %v", err)
	}
	for _, m := range []Meta{{ID: "x", Owner: "a", Slug: "b"}, {ID: repoB, Owner: "a b", Slug: "c"}, {ID: repoB, Owner: "a", Slug: ".."}, {ID: repoB, Owner: "a", Slug: "/"}} {
		if _, err := l.CreateRepo(ctx, m, "main"); err == nil {
			t.Errorf("%+v accepted", m)
		}
	}
	if _, err := l.CreateRepo(ctx, Meta{ID: repoB, Owner: "a", Slug: "b"}, "bad..branch"); err == nil {
		t.Error("bad default branch accepted")
	}
	m, err := l.ReadMeta(ctx, repoA)
	if err != nil || m.Owner != "acme" || m.Slug != "app" || m.CreatedAt.IsZero() {
		t.Fatalf("meta: %+v, %v", m, err)
	}
	if id, err := l.Resolve(ctx, "acme", "app"); err != nil || id != repoA {
		t.Fatalf("resolve: %q, %v", id, err)
	}
	if _, err := l.Resolve(ctx, "acme", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve unknown: %v", err)
	}
	if _, err := l.Resolve(ctx, "bad owner", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve invalid: %v", err)
	}
	if _, err := l.ReadMeta(ctx, "not-an-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read invalid id: %v", err)
	}

	// Rename claims the new name first, then drops the old one.
	renamed, err := l.Rename(ctx, repoA, "acme", "app2")
	if err != nil || renamed.Slug != "app2" {
		t.Fatalf("rename: %+v, %v", renamed, err)
	}
	if _, err := l.Resolve(ctx, "acme", "app"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old name still resolves: %v", err)
	}
	if id, _ := l.Resolve(ctx, "acme", "app2"); id != repoA {
		t.Fatal("new name does not resolve")
	}
	if same, err := l.Rename(ctx, repoA, "acme", "app2"); err != nil || same.Slug != "app2" {
		t.Fatalf("rename to the same name: %v", err)
	}
	if _, err := l.CreateRepo(ctx, Meta{ID: repoB, Owner: "acme", Slug: "taken"}, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Rename(ctx, repoA, "acme", "taken"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	if _, err := l.Rename(ctx, repoA, "", "x"); err == nil {
		t.Fatal("rename to an invalid label accepted")
	}
	if _, err := l.Rename(ctx, "00000000-0000-4000-8000-000000000000", "a", "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename unknown: %v", err)
	}

	// Malformed stored objects are refused rather than trusted.
	_, _ = store.Put(ctx, l.metaKey(repoA), BytesBody([]byte("{")))
	if _, err := l.ReadMeta(ctx, repoA); err == nil {
		t.Fatal("malformed meta accepted")
	}
	_, _ = store.Put(ctx, l.nameKey("acme", "app2"), BytesBody([]byte("junk")))
	if _, err := l.Resolve(ctx, "acme", "app2"); err == nil || !strings.Contains(err.Error(), "holds") {
		t.Fatalf("malformed name: %v", err)
	}

	// Store failures surface from every step of a create and a rename.
	fresh := NewMemStore()
	l = newTestLog(t, fresh)
	step := 0
	fresh.Fault = func(op, key string) error {
		if op == "Create" {
			step++
			if step == 2 {
				return errors.New("name write failed")
			}
		}
		return nil
	}
	if _, err := l.CreateRepo(ctx, Meta{ID: repoA, Owner: "a", Slug: "b"}, "main"); err == nil || !strings.Contains(err.Error(), "name write failed") {
		t.Fatalf("name step: %v", err)
	}
	fresh.Fault = func(op, key string) error {
		if op == "Create" && strings.Contains(key, "index/") {
			return errors.New("index write failed")
		}
		return nil
	}
	if _, err := l.CreateRepo(ctx, Meta{ID: repoA, Owner: "a", Slug: "b"}, "main"); err == nil || !strings.Contains(err.Error(), "index write failed") {
		t.Fatalf("index step: %v", err)
	}
	fresh.Fault = nil
	if _, err := l.CreateRepo(ctx, Meta{ID: repoB, Owner: "a", Slug: "c"}, "main"); err != nil {
		t.Fatal(err)
	}
	fresh.Fault = func(op, key string) error {
		if op == "Put" && strings.HasSuffix(key, "/meta") {
			return errors.New("meta write failed")
		}
		if op == "Delete" {
			return errors.New("delete failed")
		}
		return nil
	}
	if _, err := l.Rename(ctx, repoB, "a", "d"); err == nil || !strings.Contains(err.Error(), "meta write failed") {
		t.Fatalf("rename meta step: %v", err)
	}
	fresh.Fault = func(op, key string) error {
		if op == "Delete" {
			return errors.New("delete failed")
		}
		return nil
	}
	if _, err := l.Rename(ctx, repoB, "a", "e"); err != nil {
		t.Fatalf("rename with a failed old-name delete is still a rename: %v", err)
	}
	fresh.Fault = func(op, key string) error {
		if op == "Create" && strings.Contains(key, "names/") {
			return errors.New("name create failed")
		}
		return nil
	}
	if _, err := l.Rename(ctx, repoB, "a", "f"); err == nil || !strings.Contains(err.Error(), "name create failed") {
		t.Fatalf("rename name step: %v", err)
	}
	fresh.Fault = func(op, key string) error {
		if op == "Get" {
			return errors.New("get failed")
		}
		return nil
	}
	if _, err := l.Resolve(ctx, "a", "e"); err == nil || !strings.Contains(err.Error(), "get failed") {
		t.Fatalf("resolve get: %v", err)
	}
}

// TestWriteMetaRewritesTheObject is spec 019's unconditional rewrite:
// the administration fields round-trip, UpdatedAt is stamped, and an
// invalid id is refused before any object is written.
func TestWriteMetaRewritesTheObject(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	l := New(Options{Store: store, Now: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler)})
	if _, err := l.CreateRepo(ctx, Meta{ID: repoA, Owner: "acme", Slug: "app"}, "main"); err != nil {
		t.Fatal(err)
	}
	m, err := l.ReadMeta(ctx, repoA)
	if err != nil {
		t.Fatal(err)
	}
	frozen := now.Add(time.Minute)
	m.FrozenAt = &frozen
	m.ImportingSince = &frozen
	m.ImportNode, m.ImportError, m.ImportRefs, m.ImportBytes = "node-1", "", 7, 4096
	now = now.Add(time.Hour)
	if err := l.WriteMeta(ctx, m); err != nil {
		t.Fatal(err)
	}
	back, err := l.ReadMeta(ctx, repoA)
	if err != nil {
		t.Fatal(err)
	}
	if back.FrozenAt == nil || !back.FrozenAt.Equal(frozen) || back.ImportNode != "node-1" || back.ImportRefs != 7 || back.ImportBytes != 4096 {
		t.Fatalf("round trip: %+v", back)
	}
	if !back.UpdatedAt.Equal(now.UTC()) {
		t.Fatalf("updated_at %v, want %v", back.UpdatedAt, now.UTC())
	}
	if err := l.WriteMeta(ctx, &Meta{ID: "nope"}); err == nil {
		t.Fatal("an invalid id was written")
	}
}
