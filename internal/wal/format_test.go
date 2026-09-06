// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func validHeader() Header {
	return Header{V: 1, Kind: KindPush, Seq: 7, At: time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC), PackBytes: 3, PackSHA256: strings.Repeat("a", 64)}
}

func TestKeysRoundTrip(t *testing.T) {
	if SeqString(42) != "000000000042" || IndexKey(1043) != "index/000000001043" {
		t.Fatal("zero padding")
	}
	key := EntryKey(1043, "9f3c1a7be2d40c55")
	seq, nonce, ok := ParseEntryKey(key)
	if !ok || seq != 1043 || nonce != "9f3c1a7be2d40c55" {
		t.Fatalf("ParseEntryKey(%q) = %d %q %v", key, seq, nonce, ok)
	}
	for _, bad := range []string{"wal/1.9f3c1a7be2d40c55.entry", "wal/000000001043.xyz.entry", "index/000000001043", "wal/999999999999999.9f3c1a7be2d40c55.entry"} {
		if _, _, ok := ParseEntryKey(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if seq, ok := ParseIndexKey("index/000000000001"); !ok || seq != 1 {
		t.Fatal("ParseIndexKey")
	}
	if _, ok := ParseIndexKey("index/latest"); ok {
		t.Fatal("latest parsed as a sequence")
	}
}

func TestValidRefName(t *testing.T) {
	for _, ok := range []string{"HEAD", "refs/heads/main", "refs/tags/v1.0", "refs/heads/feature/x-y_z", "refs/heads/a.b"} {
		if !ValidRefName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "main", "refs/", "refs/heads/", "refs/heads/a..b", "refs/heads/.hidden", "refs/heads/x.lock", "refs/heads/a b", "refs/heads/a~1", "refs/heads/a//b", "refs/heads/@", "refs/heads/a@{b}", "refs/heads/a\x01", "refs/heads/a\\b", "refs/heads/a:b", "refs/heads/a?b", "refs/heads/a*", "refs/heads/a[b"} {
		if ValidRefName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseHeader(t *testing.T) {
	h := validHeader()
	line, err := EncodeEntryHead(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := bytes.SplitN(line, []byte("\n"), 2)[0]
	got, err := ParseHeader(first)
	if err != nil || got != h {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	mutate := func(f func(*Header)) []byte {
		h := validHeader()
		f(&h)
		b, _ := EncodeEntryHead(h, nil)
		return bytes.SplitN(b, []byte("\n"), 2)[0]
	}
	for name, line := range map[string][]byte{
		"version":        mutate(func(h *Header) { h.V = 2 }),
		"kind":           mutate(func(h *Header) { h.Kind = "merge" }),
		"seq zero":       mutate(func(h *Header) { h.Seq = 0 }),
		"seq huge":       mutate(func(h *Header) { h.Seq = maxSeq + 1 }),
		"negative pack":  mutate(func(h *Header) { h.PackBytes = -1 }),
		"bad sha":        mutate(func(h *Header) { h.PackSHA256 = "nope" }),
		"no time":        mutate(func(h *Header) { h.At = time.Time{} }),
		"unknown field":  []byte(`{"v":1,"kind":"push","seq":1,"at":"2026-09-06T10:00:00Z","extra":1}`),
		"not json":       []byte(`{`),
		"too long":       append([]byte(`{"v":1,"kind":"push","seq":1,"subject":"`), append(bytes.Repeat([]byte("x"), maxHeaderLine), '"', '}')...),
		"pack no sha":    []byte(`{"v":1,"kind":"push","seq":1,"at":"2026-09-06T10:00:00Z","pack_bytes":1}`),
		"delete has sha": nil,
	} {
		if line == nil {
			continue
		}
		if _, err := ParseHeader(line); err == nil {
			t.Errorf("%s accepted: %s", name, line)
		}
	}
	// A delete entry carries no pack and needs no digest.
	if _, err := ParseHeader([]byte(`{"v":1,"kind":"delete","seq":1,"at":"2026-09-06T10:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestParseTransaction(t *testing.T) {
	sha1 := strings.Repeat("1", 40)
	sha2 := strings.Repeat("2", 40)
	ok := []RefUpdate{{Ref: "refs/heads/main", Old: ZeroSHA, New: sha1}, {Ref: "HEAD", Old: "ref: refs/heads/main", New: "ref: refs/heads/dev"}, {Ref: "refs/tags/v1", Old: sha1, New: ZeroSHA}}
	line, _ := EncodeEntryHead(validHeader(), ok)
	second := bytes.SplitN(line, []byte("\n"), 3)[1]
	got, err := ParseTransaction(second)
	if err != nil || len(got) != 3 || got[1].New != "ref: refs/heads/dev" {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for name, refs := range map[string][]RefUpdate{
		"bad ref":       {{Ref: "main", Old: ZeroSHA, New: sha1}},
		"twice":         {{Ref: "refs/heads/a", Old: ZeroSHA, New: sha1}, {Ref: "refs/heads/a", Old: sha1, New: sha2}},
		"bad old":       {{Ref: "refs/heads/a", Old: "x", New: sha1}},
		"symbolic leaf": {{Ref: "refs/heads/a", Old: ZeroSHA, New: "ref: refs/heads/b"}},
		"HEAD to HEAD":  {{Ref: "HEAD", Old: "ref: refs/heads/a", New: "ref: HEAD"}},
		"no change":     {{Ref: "refs/heads/a", Old: sha1, New: sha1}},
	} {
		if err := ValidateTransaction(refs); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ParseTransaction([]byte(`[{"ref":"refs/heads/a","old":"` + ZeroSHA + `","new":"` + sha1 + `","x":1}]`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := ParseTransaction(bytes.Repeat([]byte("["), maxRefsLine+2)); err == nil {
		t.Error("oversized line accepted")
	}
}

func validIndex() *Index {
	sha := strings.Repeat("a", 40)
	return &Index{
		V: 1, Seq: 2, Entry: EntryKey(2, "9f3c1a7be2d40c55"),
		Refs:    map[string]string{"refs/heads/main": sha, "HEAD": "ref: refs/heads/main"},
		Entries: []IndexEntry{{Seq: 1, Key: EntryKey(1, "0000000000000001"), Kind: KindPush}, {Seq: 2, Key: EntryKey(2, "9f3c1a7be2d40c55"), Kind: KindPush}},
		Packs:   []string{"packs/" + sha + ".pack"}, SizeBytes: 10,
	}
}

func TestParseIndex(t *testing.T) {
	data, err := EncodeIndex(validIndex())
	if err != nil {
		t.Fatal(err)
	}
	ix, err := ParseIndex(data)
	if err != nil || ix.Seq != 2 || ix.DefaultBranch() != "main" || len(ix.Entries) != 2 {
		t.Fatalf("round trip: %+v, %v", ix, err)
	}
	zero, _ := EncodeIndex(&Index{V: 1, Refs: map[string]string{"HEAD": "ref: refs/heads/main"}})
	if _, err := ParseIndex(zero); err != nil {
		t.Fatalf("index 0: %v", err)
	}
	empty, _ := EncodeIndex(&Index{V: 1})
	if ix, err := ParseIndex(empty); err != nil || ix.Refs == nil || ix.DefaultBranch() != "" {
		t.Fatalf("empty index: %+v, %v", ix, err)
	}
	mutate := func(f func(*Index)) []byte {
		ix := validIndex()
		f(ix)
		b, _ := EncodeIndex(ix)
		return b
	}
	for name, data := range map[string][]byte{
		"version":          mutate(func(ix *Index) { ix.V = 0 }),
		"seq huge":         mutate(func(ix *Index) { ix.Seq = maxSeq + 1 }),
		"zero with entry":  mutate(func(ix *Index) { ix.Seq = 0; ix.Entries = nil }),
		"entry mismatch":   mutate(func(ix *Index) { ix.Entry = EntryKey(3, "9f3c1a7be2d40c55") }),
		"bad ref":          mutate(func(ix *Index) { ix.Refs["bad"] = ZeroSHA }),
		"bad ref value":    mutate(func(ix *Index) { ix.Refs["refs/heads/x"] = "nope" }),
		"compacted above":  mutate(func(ix *Index) { ix.CompactedThrough = 3 }),
		"entry order":      mutate(func(ix *Index) { ix.Entries[0], ix.Entries[1] = ix.Entries[1], ix.Entries[0] }),
		"entry seq":        mutate(func(ix *Index) { ix.Entries[0].Seq = 5 }),
		"entry kind":       mutate(func(ix *Index) { ix.Entries[0].Kind = "x" }),
		"entry below fold": mutate(func(ix *Index) { ix.CompactedThrough = 1 }),
		"own entry absent": mutate(func(ix *Index) { ix.Entries = ix.Entries[:1] }),
		"bad pack":         mutate(func(ix *Index) { ix.Packs = []string{"packs/x.pack"} }),
		"negative size":    mutate(func(ix *Index) { ix.SizeBytes = -1 }),
		"unknown field":    []byte(`{"v":1,"seq":0,"x":1}`),
		"not json":         []byte(`nope`),
		"too large":        bytes.Repeat([]byte(" "), maxIndexBytes+1),
	} {
		if _, err := ParseIndex(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Clone is deep.
	ix = validIndex()
	at := time.Now()
	ix.DeletedAt = &at
	c := ix.Clone()
	c.Refs["HEAD"] = "ref: refs/heads/dev"
	c.Entries[0].Seq = 99
	*c.DeletedAt = at.Add(time.Hour)
	if ix.Refs["HEAD"] != "ref: refs/heads/main" || ix.Entries[0].Seq != 1 || !ix.DeletedAt.Equal(at) {
		t.Fatal("clone shares memory with the original")
	}
}

func TestReadEntryHead(t *testing.T) {
	h := validHeader()
	refs := []RefUpdate{{Ref: "refs/heads/main", Old: ZeroSHA, New: strings.Repeat("1", 40)}}
	head, _ := EncodeEntryHead(h, refs)
	body := append(head, []byte("PACKDATA")...)
	gotH, gotRefs, pack, err := ReadEntryHead(bytes.NewReader(body))
	if err != nil || gotH != h || len(gotRefs) != 1 {
		t.Fatalf("head: %+v %+v %v", gotH, gotRefs, err)
	}
	rest, _ := io.ReadAll(pack)
	if string(rest) != "PACKDATA" {
		t.Fatalf("pack = %q", rest)
	}
	for name, in := range map[string][]byte{
		"empty":          nil,
		"header only":    head[:bytes.IndexByte(head, '\n')+1],
		"bad header":     []byte("{}\n[]\n"),
		"bad refs":       append(append([]byte{}, head[:bytes.IndexByte(head, '\n')+1]...), []byte("[{\"ref\":\"x\"}]\n")...),
		"long header":    append(bytes.Repeat([]byte("x"), 70<<10), '\n'),
		"unterminated":   head[:len(head)-1],
		"long refs line": append(append([]byte{}, head[:bytes.IndexByte(head, '\n')+1]...), bytes.Repeat([]byte("["), 70<<10)...),
	} {
		if _, _, _, err := ReadEntryHead(bytes.NewReader(in)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func FuzzParseHeader(f *testing.F) {
	line, _ := EncodeEntryHead(validHeader(), nil)
	f.Add(bytes.SplitN(line, []byte("\n"), 2)[0])
	f.Add([]byte(`{"v":1,"kind":"delete","seq":1,"at":"2026-09-06T10:00:00Z"}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHeader(data)
		if err == nil && (h.V != Version || h.Seq == 0) {
			t.Fatalf("accepted %q as %+v", data, h)
		}
	})
}

func FuzzParseTransaction(f *testing.F) {
	f.Add([]byte(`[{"ref":"refs/heads/main","old":"` + ZeroSHA + `","new":"` + strings.Repeat("1", 40) + `"}]`))
	f.Add([]byte(`[{"ref":"HEAD","old":"ref: refs/heads/a","new":"ref: refs/heads/b"}]`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		refs, err := ParseTransaction(data)
		if err != nil {
			return
		}
		for _, u := range refs {
			if !ValidRefName(u.Ref) || u.Old == u.New {
				t.Fatalf("accepted %+v", u)
			}
		}
	})
}

func FuzzParseIndex(f *testing.F) {
	data, _ := EncodeIndex(validIndex())
	f.Add(data)
	zero, _ := EncodeIndex(&Index{V: 1, Refs: map[string]string{"HEAD": "ref: refs/heads/main"}})
	f.Add(zero)
	f.Add([]byte(`{"v":1}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		ix, err := ParseIndex(data)
		if err != nil {
			return
		}
		if ix.V != Version || ix.CompactedThrough > ix.Seq {
			t.Fatalf("accepted %q as %+v", data, ix)
		}
		if _, err := EncodeIndex(ix); err != nil {
			t.Fatal(err)
		}
	})
}
