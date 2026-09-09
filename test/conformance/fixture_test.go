// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/wal"
	"github.com/latere-ai/origo/test/conformance"
	"github.com/latere-ai/origo/test/stubs/origo"
)

// TestReleaseFixtureRoundTrip is the harness of spec 017 against the
// contract stub: the fixture pushed through one node is packed from
// its bucket as the manifest and every object under its prefix, an
// upload under a fresh id puts the same objects under that id and
// nothing else, the node materializes the copy from the log alone with
// the same rev-list --all, and a run of the suite leaves the fixture in
// place because its slug is outside the conformance- prefix.
func TestReleaseFixtureRoundTrip(t *testing.T) {
	stub := origo.New(t)
	ctx := context.Background()
	target := conformance.Target{URL: stub.URL(), Token: stub.Token("dev", "")}
	f := conformance.PushReleaseFixture(t, target)
	if f.Version != "dev" || f.Owner != conformance.Owner || strings.HasPrefix(f.Slug, conformance.SlugPrefix) || len(f.RevList) < 4 {
		t.Fatalf("manifest %+v", f)
	}
	if !slices.IsSorted(f.RevList) {
		t.Fatalf("rev-list is not sorted: %v", f.RevList)
	}

	var archive bytes.Buffer
	if err := conformance.PackReleaseFixture(ctx, stub.Store(), config.Prefix, f, &archive); err != nil {
		t.Fatal(err)
	}
	names := entries(t, archive.Bytes())
	if names[0] != conformance.ReleaseFixtureManifest || !slices.Contains(names, "objects/meta") || !slices.Contains(names, "objects/index/000000000001") {
		t.Fatalf("archive entries %v", names)
	}
	keys := list(t, stub.Store(), f.Prefix(config.Prefix))
	if len(names)-1 != len(keys) {
		t.Fatalf("%d objects in the archive, %d under the prefix", len(names)-1, len(keys))
	}
	path := filepath.Join(t.TempDir(), "fixture-dev.tar.gz")
	if err := os.WriteFile(path, archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	id := newID(t)
	uploaded, err := conformance.UploadReleaseFixture(ctx, stub.Store(), config.Prefix, path, id)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.ID != id || !slices.Equal(uploaded.RevList, f.RevList) || uploaded.Version != f.Version {
		t.Fatalf("uploaded manifest %+v", uploaded)
	}
	copied := list(t, stub.Store(), conformance.RepoPrefix(config.Prefix, id))
	if len(copied) != len(keys) {
		t.Fatalf("%d objects under the copy, %d under the original", len(copied), len(keys))
	}
	if got := conformance.RevList(t, stub.URL(), stub.Token("dev", ""), id); !slices.Equal(got, f.RevList) {
		t.Fatalf("rev-list of the copy %v, want %v", got, f.RevList)
	}
	// The archive is what it was.
	if b, _ := os.ReadFile(path); !bytes.Equal(b, archive.Bytes()) {
		t.Fatal("the archive was modified")
	}
	// The copy is removed the way TestPreviousReleaseFixture removes
	// it, and the original stays.
	if err := conformance.DeletePrefix(ctx, stub.Store(), conformance.RepoPrefix(config.Prefix, id)); err != nil {
		t.Fatal(err)
	}
	if left := list(t, stub.Store(), conformance.RepoPrefix(config.Prefix, id)); len(left) != 0 {
		t.Fatalf("%d objects left under the copy", len(left))
	}
	if got := conformance.RevList(t, stub.URL(), stub.Token("dev", ""), f.ID); !slices.Equal(got, f.RevList) {
		t.Fatalf("the original after the delete: %v", got)
	}

	// A run of the suite creates and deletes its own repositories and
	// leaves the fixture.
	report := conformance.Run(t, conformance.Target{URL: stub.URL(), Token: stub.Token("dev", ""), Issuer: stub.Issuer().URL(), Authorizer: stub.Authorizer().URL(), EventsSink: stub.Sink().URL(), Fault: stub.Fault()})
	if len(report.Failed) != 0 || slices.Contains(report.Created, f.ID) {
		t.Fatalf("the run failed %v or created the fixture %v", report.Failed, report.Created)
	}
	if got := conformance.RevList(t, stub.URL(), stub.Token("dev", ""), f.ID); !slices.Equal(got, f.RevList) {
		t.Fatalf("the fixture after a run: %v", got)
	}
}

// TestReleaseFixtureRefusesABadArchive: an archive without a manifest
// or objects, a path that escapes the prefix, an unknown entry, and a
// file that is not an archive are refused before anything is put, and
// an empty manifest is not packed.
func TestReleaseFixtureRefusesABadArchive(t *testing.T) {
	ctx := context.Background()
	store := wal.NewMemStore()
	write := func(name string, entries map[string]string) string {
		t.Helper()
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for n, data := range entries {
			if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(data))}); err != nil {
				t.Fatal(err)
			}
			_, _ = tw.Write([]byte(data))
		}
		_ = tw.Close()
		_ = gz.Close()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	manifest, _ := json.Marshal(conformance.ReleaseFixture{ID: "x", RevList: []string{"a"}})
	id := newID(t)
	for name, path := range map[string]string{
		"no manifest": write("a.tar.gz", map[string]string{"objects/meta": "{}"}),
		"no objects":  write("b.tar.gz", map[string]string{conformance.ReleaseFixtureManifest: string(manifest)}),
		"escape":      write("c.tar.gz", map[string]string{conformance.ReleaseFixtureManifest: string(manifest), "objects/../../meta": "{}"}),
		"unknown":     write("d.tar.gz", map[string]string{conformance.ReleaseFixtureManifest: string(manifest), "notes.txt": "hi"}),
		"not json":    write("e.tar.gz", map[string]string{conformance.ReleaseFixtureManifest: "{"}),
		"not gzip":    writeRaw(t, "plain"),
		"missing":     filepath.Join(t.TempDir(), "none.tar.gz"),
	} {
		if _, err := conformance.UploadReleaseFixture(ctx, store, config.Prefix, path, id); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if keys := list(t, store, config.Prefix); len(keys) != 0 {
		t.Fatalf("a refused archive put %v", keys)
	}
	var out bytes.Buffer
	if err := conformance.PackReleaseFixture(ctx, store, config.Prefix, conformance.ReleaseFixture{ID: id}, &out); err == nil {
		t.Fatal("an empty manifest was packed")
	}
	if err := conformance.PackReleaseFixture(ctx, store, config.Prefix, conformance.ReleaseFixture{ID: id, RevList: []string{"a"}}, &out); err == nil {
		t.Fatal("a prefix with nothing under it was packed")
	}
	// A store that fails is reported, not hidden.
	mem := wal.NewMemStore()
	if _, err := mem.Put(ctx, conformance.RepoPrefix(config.Prefix, id)+"meta", wal.BytesBody([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	mem.SetFault(func(op, _ string) error {
		if op == "Get" {
			return errors.New("unreachable")
		}
		return nil
	})
	if err := conformance.PackReleaseFixture(ctx, mem, config.Prefix, conformance.ReleaseFixture{ID: id, RevList: []string{"a"}}, &out); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("a failing get: %v", err)
	}
	mem.SetFault(func(op, _ string) error { return errors.New("unreachable") })
	if err := conformance.DeletePrefix(ctx, mem, config.Prefix); err == nil {
		t.Fatal("a failing list was hidden")
	}
}

func writeRaw(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.tar.gz")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// entries lists an archive's entry names in order.
func entries(t *testing.T, archive []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// list answers every key under a prefix.
func list(t *testing.T, store wal.Store, prefix string) []string {
	t.Helper()
	var keys []string
	after := ""
	for {
		page, err := store.List(context.Background(), wal.ListOptions{Prefix: prefix, StartAfter: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Objects {
			keys = append(keys, o.Key)
			after = o.Key
		}
		if !page.Truncated || len(page.Objects) == 0 {
			return keys
		}
	}
}
