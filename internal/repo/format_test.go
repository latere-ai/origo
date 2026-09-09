// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/internal/metrics"
	"github.com/latere-ai/origo/internal/wal"
)

// TestNewerLogFormatIsRefused is spec 017's upgrade criterion: a node
// that reads an index object, or an entry header, whose v is above what
// it reads refuses that repository as an integrity error naming the
// object, which the handlers answer 503 repository_unavailable with
// details.key (spec 015), logs the documented line, serves another
// repository, and stays ready: no breaker opens, because the bucket
// answered.
func TestNewerLogFormatIsRefused(t *testing.T) {
	ctx := context.Background()
	var lines bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&lines, nil))
	mem := wal.NewMemStore()
	reg := pkgmetrics.NewRegistry()
	set := metrics.Register(reg)
	store := wal.NewBreakerStore(wal.BreakerOptions{Store: mem, Metrics: set, Threshold: 1})
	l := wal.New(wal.Options{Store: store, Logger: logger, Metrics: set})
	c, err := New(Options{Dir: filepath.Join(t.TempDir(), "data"), Log: l, Logger: logger, Metrics: set, StaleMax: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ix, err := l.CreateRepo(ctx, wal.Meta{ID: repoA, Owner: "acme", Slug: "app"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.CreateRepo(ctx, wal.Meta{ID: repoB, Owner: "acme", Slug: "other"}, "main"); err != nil {
		t.Fatal(err)
	}
	src := gittest.NewSource(t)
	c1 := src.Commit("a.txt", "one", "first")
	commit, err := l.Commit(ctx, repoA, ix, wal.Entry{Kind: wal.KindPush, Refs: []wal.RefUpdate{{Ref: "refs/heads/main", Old: wal.ZeroSHA, New: c1}}, Pack: wal.BytesBody(src.Pack(c1))}, func(context.Context, *wal.Index) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	read := func(key string) []byte {
		t.Helper()
		rc, _, err := mem.Get(ctx, key, "")
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		return b
	}
	rewrite := func(key string, from, to string) {
		t.Helper()
		b := bytes.Replace(read(key), []byte(from), []byte(to), 1)
		if _, err := mem.Put(ctx, key, wal.BytesBody(b)); err != nil {
			t.Fatal(err)
		}
	}
	const line = "log format v2 is newer than this release reads; see docs/upgrades/2.md"
	refused := func(key string) {
		t.Helper()
		lines.Reset()
		_, err := c.Lease(ctx, repoA, false)
		ie, ok := errors.AsType[*wal.IntegrityError](err)
		if !ok || ie.Key != key {
			t.Fatalf("lease under a newer format: %v", err)
		}
		if _, ok := errors.AsType[*wal.NewerFormatError](err); !ok {
			t.Fatalf("not a NewerFormatError: %v", err)
		}
		if !strings.Contains(lines.String(), `msg="`+line+`"`) || !strings.Contains(lines.String(), key) {
			t.Fatalf("the documented line was not logged:\n%s", lines.String())
		}
		// Another repository is served, and readiness holds: neither
		// breaker opened, because every call the bucket answered.
		if lease, err := c.Lease(ctx, repoB, false); err != nil {
			t.Fatalf("the other repository: %v", err)
		} else {
			lease.Release()
		}
		for _, class := range []wal.Class{wal.ClassRead, wal.ClassWrite} {
			if !store.Admits(class) {
				t.Fatalf("the %s breaker opened", class)
			}
		}
	}
	// The index object first, on a cold copy.
	indexKey := l.RepoPrefix(repoA) + wal.IndexKey(commit.Index.Seq)
	original := read(indexKey)
	rewrite(indexKey, `"v":1`, `"v":2`)
	refused(indexKey)
	// Restored, the repository serves; then the entry's header, on a
	// copy evicted so the entry is read again.
	if _, err := mem.Put(ctx, indexKey, wal.BytesBody(original)); err != nil {
		t.Fatal(err)
	}
	if lease, err := c.Lease(ctx, repoA, false); err != nil {
		t.Fatalf("after the restore: %v", err)
	} else {
		lease.Release()
	}
	c.Evict(repoA)
	entryKey := l.RepoPrefix(repoA) + commit.Index.Entry
	rewrite(entryKey, `{"v":1,`, `{"v":2,`)
	refused(entryKey)
	if got := metricValue(t, reg, "origo_log_integrity_errors_total"); got != "2" {
		t.Fatalf("integrity errors %q", got)
	}
}

func metricValue(t *testing.T, reg *pkgmetrics.Registry, name string) string {
	t.Helper()
	var text bytes.Buffer
	reg.WritePrometheus(&text)
	for line := range strings.SplitSeq(text.String(), "\n") {
		if after, ok := strings.CutPrefix(line, name+" "); ok {
			return after
		}
	}
	return ""
}
