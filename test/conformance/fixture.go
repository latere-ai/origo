// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/wal"
)

// The release fixture of spec 017: a repository one release pushes
// through the stack and attaches to its GitHub release as the bucket
// prefix origo/repos/<id>/, so the next release proves it reads what
// this one wrote. PushReleaseFixture pushes it and answers its
// manifest, PackReleaseFixture writes the archive from the bucket,
// UploadReleaseFixture puts an archive's objects under a fresh prefix,
// and RevList is the comparison both sides make.

// ReleaseFixtureManifest is the archive's manifest entry.
const ReleaseFixtureManifest = "fixture.json"

// fixtureObjects is the archive directory the objects sit under, each
// at its key relative to the repository prefix.
const fixtureObjects = "objects/"

// ReleaseFixture is the manifest of a release fixture: the repository the
// objects belong to, the release that wrote it, and git rev-list --all
// of the repository as it was served, sorted.
type ReleaseFixture struct {
	ID      string   `json:"id"`
	Owner   string   `json:"owner"`
	Slug    string   `json:"slug"`
	Version string   `json:"version"`
	RevList []string `json:"rev_list"`
}

// Prefix is the repository's key prefix under the log prefix.
func (f ReleaseFixture) Prefix(logPrefix string) string { return RepoPrefix(logPrefix, f.ID) }

// RepoPrefix is the key prefix of one repository under a log prefix,
// spec 004's origo/repos/<id>/.
func RepoPrefix(logPrefix, id string) string { return logPrefix + "repos/" + id + "/" }

// PushFixture creates the fixture repository on the target under a
// slug outside the conformance- prefix, so a run's cleanup never meets
// it, pushes a history with two branches and two tags, and answers the
// manifest with the target's served version and the rev-list a mirror
// clone reads back.
func PushReleaseFixture(t testing.TB, target Target) ReleaseFixture {
	t.Helper()
	s := &session{target: target, client: &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Minute}}
	id := newID(t)
	slug := "fixture-" + id[:8]
	r := s.call(t, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, Owner, slug))
	expectStatus(t, r, http.StatusCreated)
	work := initRepo(t, s.repoURL(id))
	commitFile(t, work, "README.md", []byte("the release fixture\n"), "first")
	commitFile(t, work, "a/b.txt", []byte("two\n"), "second")
	mustGit(t, work, "tag", "v1")
	commitFile(t, work, "c.txt", []byte("three\n"), "third")
	mustGit(t, work, "tag", "-a", "release", "-m", "the annotated tag")
	mustGit(t, work, "checkout", "-q", "-b", "feature", "HEAD~1")
	commitFile(t, work, "feature.txt", []byte("four\n"), "fourth")
	mustGit(t, work, "push", "-q", "origin", "main", "feature", "v1", "release")
	version := s.call(t, "GET", "/version", "")
	expectStatus(t, version, http.StatusOK)
	served, _ := version.json["version"].(string)
	return ReleaseFixture{ID: id, Owner: Owner, Slug: slug, Version: served, RevList: RevList(t, target.URL, target.Token, id)}
}

// RevList is git rev-list --all of the repository at id as the target
// serves it, read through a mirror clone and sorted, so two releases
// compare the same thing.
func RevList(t testing.TB, url, token, id string) []string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mirror")
	mustGit(t, t.TempDir(), "clone", "-q", "--mirror", withCredential(url, token)+"/r/"+id+".git", dir)
	out := mustGit(t, dir, "rev-list", "--all")
	revs := strings.Fields(out)
	sort.Strings(revs)
	return revs
}

// PackFixture writes the archive: the manifest, then every object
// under the fixture's prefix at its relative key under objects/. The
// listing is paged through the store's own limit.
func PackReleaseFixture(ctx context.Context, store wal.Store, logPrefix string, f ReleaseFixture, w io.Writer) (err error) {
	if len(f.RevList) == 0 {
		return errors.New("fixture: the manifest has no rev-list")
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	defer func() {
		if cerr := tw.Close(); err == nil {
			err = cerr
		}
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
	}()
	manifest, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := writeEntry(tw, ReleaseFixtureManifest, manifest); err != nil {
		return err
	}
	prefix := f.Prefix(logPrefix)
	after := ""
	n := 0
	for {
		page, err := store.List(ctx, wal.ListOptions{Prefix: prefix, StartAfter: after})
		if err != nil {
			return fmt.Errorf("fixture: list %s: %w", prefix, err)
		}
		for _, o := range page.Objects {
			rc, _, err := store.Get(ctx, o.Key, "")
			if err != nil {
				return fmt.Errorf("fixture: get %s: %w", o.Key, err)
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return fmt.Errorf("fixture: read %s: %w", o.Key, err)
			}
			if err := writeEntry(tw, fixtureObjects+strings.TrimPrefix(o.Key, prefix), data); err != nil {
				return err
			}
			after = o.Key
			n++
		}
		if !page.Truncated || len(page.Objects) == 0 {
			break
		}
	}
	if n == 0 {
		return fmt.Errorf("fixture: nothing under %s", prefix)
	}
	return nil
}

func writeEntry(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0).UTC()}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// UploadFixture reads the archive at path and puts every object under
// the prefix of id, a fresh id the caller drew, so the nodes materialize
// the repository from the log the previous release wrote and the
// archive itself is never modified. It answers the manifest with ID
// set to the new id.
func UploadReleaseFixture(ctx context.Context, store wal.Store, logPrefix, archive, id string) (ReleaseFixture, error) {
	f, err := os.Open(archive)
	if err != nil {
		return ReleaseFixture{}, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return ReleaseFixture{}, fmt.Errorf("fixture: %s: %w", archive, err)
	}
	tr := tar.NewReader(gz)
	var manifest ReleaseFixture
	seen := false
	type object struct {
		rel  string
		data []byte
	}
	// The whole archive is read and checked before the first put, so a
	// bad archive puts nothing.
	var objects []object
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ReleaseFixture{}, fmt.Errorf("fixture: %s: %w", archive, err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return ReleaseFixture{}, fmt.Errorf("fixture: %s: %w", h.Name, err)
		}
		switch {
		case h.Name == ReleaseFixtureManifest:
			if err := json.Unmarshal(data, &manifest); err != nil {
				return ReleaseFixture{}, fmt.Errorf("fixture: %s: %w", h.Name, err)
			}
			seen = true
		case strings.HasPrefix(h.Name, fixtureObjects) && h.Typeflag == tar.TypeReg:
			rel := strings.TrimPrefix(h.Name, fixtureObjects)
			if rel == "" || path.Clean(rel) != rel || strings.HasPrefix(rel, "../") {
				return ReleaseFixture{}, fmt.Errorf("fixture: refusing the entry %q", h.Name)
			}
			objects = append(objects, object{rel: rel, data: bytes.Clone(data)})
		default:
			return ReleaseFixture{}, fmt.Errorf("fixture: unexpected entry %q", h.Name)
		}
	}
	if !seen || len(objects) == 0 {
		return ReleaseFixture{}, fmt.Errorf("fixture: %s holds no manifest or no objects", archive)
	}
	prefix := RepoPrefix(logPrefix, id)
	for _, o := range objects {
		if _, err := store.Put(ctx, prefix+o.rel, wal.BytesBody(o.data)); err != nil {
			return ReleaseFixture{}, fmt.Errorf("fixture: put %s: %w", prefix+o.rel, err)
		}
	}
	manifest.ID = id
	return manifest, nil
}

// DeletePrefix removes every object under a prefix, the way a test
// removes the copy it uploaded.
func DeletePrefix(ctx context.Context, store wal.Store, prefix string) error {
	after := ""
	for {
		page, err := store.List(ctx, wal.ListOptions{Prefix: prefix, StartAfter: after})
		if err != nil {
			return err
		}
		for _, o := range page.Objects {
			if err := store.Delete(ctx, o.Key); err != nil {
				return err
			}
			after = o.Key
		}
		if !page.Truncated || len(page.Objects) == 0 {
			return nil
		}
	}
}
