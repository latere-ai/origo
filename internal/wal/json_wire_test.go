// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
)

// An index object is stored and compared by content, so its bytes are
// fixed: keys of refs sorted, nil collections written as empty ones.
func TestEncodeIndexBytesAreExact(t *testing.T) {
	ix := &Index{
		V: Version, Seq: 0,
		Refs: map[string]string{"refs/heads/z": ZeroSHA, "HEAD": "ref: refs/heads/main", "refs/heads/a": ZeroSHA},
	}
	got, err := EncodeIndex(ix)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"seq":0,"entry":"","refs":{"HEAD":"ref: refs/heads/main","refs/heads/a":"` + ZeroSHA + `","refs/heads/z":"` + ZeroSHA + `"},"entries":[],"packs":[],"compacted_through":0,"size_bytes":0,"deleted_at":null,"pushed_at":null}`
	if string(got) != want {
		t.Fatalf("index bytes\n got %s\nwant %s", got, want)
	}
	if _, err := ParseIndex(got); err != nil {
		t.Fatalf("an index this package wrote is refused: %v", err)
	}
}

// An entry header omits an empty push_options and pack_bytes of a row, and
// a reader still refuses a name it does not know.
func TestHeaderAndEntryHeadBytesAreExact(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	h := Header{V: Version, Kind: KindPush, Seq: 3, At: at, Subject: "iss|alice <a&b>", PackBytes: 12, PackSHA256: "ab"}
	head, err := EncodeEntryHead(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"kind":"push","seq":3,"at":"2026-09-01T10:00:00Z","subject":"iss|alice <a&b>","pack_bytes":12,"pack_sha256":"ab"}` + "\n[]\n"
	if string(head) != want {
		t.Fatalf("entry head\n got %s\nwant %s", head, want)
	}
	ix := &Index{V: Version, Entries: []IndexEntry{{Seq: 1, Key: "k", Kind: KindPush}}}
	got, err := EncodeIndex(ix)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"entries":[{"seq":1,"key":"k","kind":"push","pack_sha256":""}]`; !strings.Contains(string(got), want) {
		t.Fatalf("a row without pack bytes carries pack_bytes: %s", got)
	}
	if _, err := ParseHeader([]byte(`{"v":1,"kind":"push","seq":1,"at":"2026-09-01T10:00:00Z","extra":1}`)); err == nil {
		t.Fatal("a header with an unknown name is read")
	}
	legacy := `{"v":1,"kind":"push","seq":1,"at":"2026-09-01T10:00:00Z","subject":"s","pack_bytes":0,"pack_sha256":"","actor":"x"}`
	if _, err := ParseHeader([]byte(legacy)); err != nil {
		t.Fatalf("a header with the retired actor is refused: %v", err)
	}
}

// Metadata omits the administration state until it is set, sorted as
// declared, and keeps a present timestamp.
func TestMetaBytesAreExact(t *testing.T) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	m := Meta{ID: "i", Owner: "o", Slug: "s", CreatedAt: at, UpdatedAt: at}
	got, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"i","owner":"o","slug":"s","created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z"}`
	if string(got) != want {
		t.Fatalf("meta bytes\n got %s\nwant %s", got, want)
	}
	frozen := at.Add(time.Hour)
	verified := true
	m.FrozenAt, m.VerifiedEqual, m.ImportRefs = &frozen, &verified, 4
	got, err = json.Marshal(m, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"id":"i","owner":"o","slug":"s","created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z","frozen_at":"2026-09-01T11:00:00Z","import_refs":4,"verified_equal":true}`
	if string(got) != want {
		t.Fatalf("meta bytes\n got %s\nwant %s", got, want)
	}
}

// A push option is bytes the git client chose; the entry head keeps the
// push and writes U+FFFD for the bytes that are not UTF-8.
func TestEntryHeadKeepsInvalidUTF8PushOption(t *testing.T) {
	h := validHeader()
	h.PushOptions = []string{"note=\xff"}
	got, err := EncodeEntryHead(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"push_options":["note=`+"�"+`"]`) {
		t.Fatalf("head %s", got)
	}
}
