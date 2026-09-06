// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// runStoreSuite exercises exactly the primitives spec 004 relies on. The
// in-process fake runs it in the unit tier; the integration tier runs it
// against MinIO with the same expectations.
func runStoreSuite(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("create refuses an existing key and leaves it untouched", func(t *testing.T) {
		s := newStore(t)
		etag, err := s.Create(ctx, "idx/1", BytesBody([]byte("a")))
		if err != nil || etag == "" {
			t.Fatalf("create: %q, %v", etag, err)
		}
		if _, err := s.Create(ctx, "idx/1", BytesBody([]byte("b"))); !errors.Is(err, ErrExists) {
			t.Fatalf("second create: %v", err)
		}
		if got := read(ctx, t, s, "idx/1"); got != "a" {
			t.Fatalf("content = %q after a refused create", got)
		}
	})

	t.Run("put overwrites and the etag changes", func(t *testing.T) {
		s := newStore(t)
		e1, err := s.Put(ctx, "hint", BytesBody([]byte("1")))
		if err != nil {
			t.Fatal(err)
		}
		e2, err := s.Put(ctx, "hint", BytesBody([]byte("2")))
		if err != nil || e1 == e2 {
			t.Fatalf("etags %q %q, %v", e1, e2, err)
		}
		if got := read(ctx, t, s, "hint"); got != "2" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("head answers 404 or the etag get reports", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Head(ctx, "absent"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("head absent: %v", err)
		}
		if _, _, err := s.Get(ctx, "absent", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("get absent: %v", err)
		}
		if _, err := s.Create(ctx, "k", BytesBody([]byte("body"))); err != nil {
			t.Fatal(err)
		}
		h, err := s.Head(ctx, "k")
		if err != nil || h.Size != 4 || h.ETag == "" || h.LastModified.IsZero() {
			t.Fatalf("head: %+v, %v", h, err)
		}
		rc, o, err := s.Get(ctx, "k", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		if o.ETag != h.ETag || o.Size != 4 {
			t.Fatalf("get object %+v, head %+v", o, h)
		}
	})

	t.Run("conditional get answers 304 for the current etag and 200 for a stale one", func(t *testing.T) {
		s := newStore(t)
		etag, err := s.Create(ctx, "k", BytesBody([]byte("body")))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Get(ctx, "k", etag); !errors.Is(err, ErrNotModified) {
			t.Fatalf("get current: %v", err)
		}
		rc, _, err := s.Get(ctx, "k", `"0123456789abcdef0123456789abcdef"`)
		if err != nil {
			t.Fatalf("get stale: %v", err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(b) != "body" {
			t.Fatalf("body = %q", b)
		}
	})

	t.Run("list is lexical with prefix, start-after, max, and delimiter", func(t *testing.T) {
		s := newStore(t)
		for _, k := range []string{"r/a/index/000000000002", "r/a/index/000000000001", "r/a/index/latest", "r/b/meta", "r/a/wal/x"} {
			if _, err := s.Put(ctx, k, BytesBody([]byte(k))); err != nil {
				t.Fatal(err)
			}
		}
		res, err := s.List(ctx, ListOptions{Prefix: "r/a/index/0"})
		if err != nil {
			t.Fatal(err)
		}
		if keys(res) != "r/a/index/000000000001,r/a/index/000000000002" || res.Truncated {
			t.Fatalf("list = %q truncated %v", keys(res), res.Truncated)
		}
		res, err = s.List(ctx, ListOptions{Prefix: "r/a/", Max: 2})
		if err != nil || !res.Truncated || keys(res) != "r/a/index/000000000001,r/a/index/000000000002" {
			t.Fatalf("page 1 = %q truncated %v, %v", keys(res), res.Truncated, err)
		}
		res, err = s.List(ctx, ListOptions{Prefix: "r/a/", Max: 2, StartAfter: "r/a/index/000000000002"})
		if err != nil || keys(res) != "r/a/index/latest,r/a/wal/x" {
			t.Fatalf("page 2 = %q, %v", keys(res), err)
		}
		res, err = s.List(ctx, ListOptions{Prefix: "r/", Delimiter: "/"})
		if err != nil || len(res.Objects) != 0 || strings.Join(res.Prefixes, ",") != "r/a/,r/b/" {
			t.Fatalf("prefixes = %q objects %d, %v", res.Prefixes, len(res.Objects), err)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Put(ctx, "k", BytesBody([]byte("x"))); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "k"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "k"); err != nil {
			t.Fatalf("second delete: %v", err)
		}
		if _, err := s.Head(ctx, "k"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("head after delete: %v", err)
		}
	})

	t.Run("16 writers race to create one key for 20 rounds, exactly one wins each", func(t *testing.T) {
		s := newStore(t)
		const writers, rounds = 16, 20
		for round := range rounds {
			key := fmt.Sprintf("race/%012d", round)
			var wg sync.WaitGroup
			wins := make([]int, writers)
			for w := range writers {
				wg.Go(func() {
					_, err := s.Create(ctx, key, BytesBody(fmt.Appendf(nil, "writer-%d", w)))
					switch {
					case err == nil:
						wins[w] = 1
					case errors.Is(err, ErrExists):
					default:
						t.Errorf("round %d writer %d: %v", round, w, err)
					}
				})
			}
			wg.Wait()
			winner, total := -1, 0
			for w, n := range wins {
				total += n
				if n == 1 {
					winner = w
				}
			}
			if total != 1 {
				t.Fatalf("round %d: %d winners", round, total)
			}
			if got := read(ctx, t, s, key); got != fmt.Sprintf("writer-%d", winner) {
				t.Fatalf("round %d: stored %q, winner %d", round, got, winner)
			}
		}
	})
}

func read(ctx context.Context, t *testing.T, s Store, key string) string {
	t.Helper()
	rc, _, err := s.Get(ctx, key, "")
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func keys(res ListResult) string {
	var b []string
	for _, o := range res.Objects {
		b = append(b, o.Key)
	}
	return strings.Join(b, ",")
}

func TestMemStoreSuite(t *testing.T) {
	runStoreSuite(t, func(*testing.T) Store { return NewMemStore() })
}

func TestMemStoreFaultsAndHelpers(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	boom := errors.New("boom")
	m.Fault = func(op, key string) error {
		if key == "bad" {
			return boom
		}
		return nil
	}
	for _, op := range []func() error{
		func() error { _, err := m.Create(ctx, "bad", BytesBody(nil)); return err },
		func() error { _, err := m.Put(ctx, "bad", BytesBody(nil)); return err },
		func() error { _, _, err := m.Get(ctx, "bad", ""); return err },
		func() error { _, err := m.Head(ctx, "bad"); return err },
		func() error { _, err := m.List(ctx, ListOptions{Prefix: "bad"}); return err },
		func() error { return m.Delete(ctx, "bad") },
	} {
		if err := op(); !errors.Is(err, boom) {
			t.Fatalf("fault not applied: %v", err)
		}
	}
	// A lost response applies the write and reports the loss.
	m.Fault = func(op, key string) error { return ErrLostResponse }
	if _, err := m.Create(ctx, "k", BytesBody([]byte("v"))); !errors.Is(err, ErrLostResponse) {
		t.Fatalf("create: %v", err)
	}
	m.Fault = nil
	if read(ctx, t, m, "k") != "v" {
		t.Fatal("lost create not applied")
	}
	m.Fault = func(op, key string) error { return ErrLostResponse }
	if _, err := m.Put(ctx, "k", BytesBody([]byte("w"))); !errors.Is(err, ErrLostResponse) {
		t.Fatalf("put: %v", err)
	}
	// A lost create on an existing key reports the existence, not the loss.
	if _, err := m.Create(ctx, "k", BytesBody([]byte("z"))); !errors.Is(err, ErrExists) {
		t.Fatalf("lost create on existing: %v", err)
	}
	m.Fault = nil
	if read(ctx, t, m, "k") != "w" {
		t.Fatal("lost put not applied")
	}
	if m.Calls["Create"] < 2 || m.Calls["Put"] < 1 {
		t.Fatalf("calls = %v", m.Calls)
	}
	if got := strings.Join(m.Keys(), ","); got != "k" {
		t.Fatalf("keys = %q", got)
	}
	// A body whose size disagrees with its content is refused.
	bad := Body{Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte("abc"))), nil }, Size: 2}
	if _, err := m.Put(ctx, "size", bad); err == nil {
		t.Fatal("size mismatch accepted")
	}
	failing := Body{Open: func() (io.ReadCloser, error) { return nil, boom }, Size: 1}
	if _, err := m.Put(ctx, "open", failing); !errors.Is(err, boom) {
		t.Fatalf("open failure: %v", err)
	}
	m.Touch("k", m.now().Add(-time.Hour))
	m.Touch("missing", m.now())
	h, _ := m.Head(ctx, "k")
	if m.now().Sub(h.LastModified) < time.Hour {
		t.Fatal("touch did not age the object")
	}
	m.SetClock(func() time.Time { return time.Unix(0, 0) })
	if _, err := m.Put(ctx, "epoch", BytesBody(nil)); err != nil {
		t.Fatal(err)
	}
	if h, _ := m.Head(ctx, "epoch"); !h.LastModified.Equal(time.Unix(0, 0)) {
		t.Fatalf("clock not used: %v", h.LastModified)
	}
	// Max is capped and a listing that overflows on a prefix is truncated.
	for i := range 3 {
		_, _ = m.Put(ctx, fmt.Sprintf("d/%d/x", i), BytesBody(nil))
	}
	res, _ := m.List(ctx, ListOptions{Prefix: "d/", Delimiter: "/", Max: 2})
	if len(res.Prefixes) != 2 || !res.Truncated {
		t.Fatalf("delimiter page: %+v", res)
	}
	res, _ = m.List(ctx, ListOptions{Prefix: "d/", Max: 5000})
	if len(res.Objects) != 3 {
		t.Fatalf("capped max: %+v", res)
	}
}

func TestFileBodyHashesOnce(t *testing.T) {
	path := t.TempDir() + "/pack"
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := FileBody(path)
	if err != nil {
		t.Fatal(err)
	}
	want := BytesBody([]byte("hello"))
	if b.Size != 5 || b.SHA256 != want.SHA256 || b.MD5 != want.MD5 {
		t.Fatalf("file body %+v, bytes body %+v", b, want)
	}
	if got, _ := b.ReadAll(); string(got) != "hello" {
		t.Fatalf("read = %q", got)
	}
	if _, err := FileBody(path + ".missing"); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := FileBody(t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
}
