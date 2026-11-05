// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var benchAt = time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)

// benchHex returns a deterministic lower-case hex string of n digits.
func benchHex(seed, n int) string {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		fmt.Fprintf(&sb, "%016x", uint64(seed+1)*0x9e3779b97f4a7c15+uint64(i)*0xbf58476d1ce4e5b9)
	}
	return sb.String()[:n]
}

// benchIndex builds a valid index object with the given number of
// entries since the last compaction and refs.
func benchIndex(entries, refs int) *Index {
	ix := &Index{
		V: Version, Seq: uint64(entries), Refs: map[string]string{"HEAD": "ref: refs/heads/main"},
		Entries: make([]IndexEntry, 0, entries), Packs: []string{},
	}
	for i := 1; i <= entries; i++ {
		key := EntryKey(uint64(i), benchHex(i, 16))
		ix.Entries = append(ix.Entries, IndexEntry{
			Seq: uint64(i), Key: key, Kind: KindPush, PackBytes: int64(4096 + i*37), PackSHA256: benchHex(i, 64),
		})
		ix.SizeBytes += int64(4096 + i*37)
	}
	ix.Entry = ix.Entries[len(ix.Entries)-1].Key
	for i := range refs {
		ix.Refs[fmt.Sprintf("refs/heads/feature/topic-%04d", i)] = benchHex(i+1000, 40)
	}
	for i := range 4 {
		ix.Packs = append(ix.Packs, "packs/"+benchHex(i+5000, 40)+".pack")
	}
	pushed := benchAt
	ix.PushedAt = &pushed
	return ix
}

// benchIndexSizes are the index objects a small, a busy, and a
// long-uncompacted repository write.
var benchIndexSizes = []struct {
	name          string
	entries, refs int
}{
	{"small", 2, 3},
	{"medium", 40, 20},
	{"large", 900, 100},
}

func BenchmarkJSONIndexUnmarshal(b *testing.B) {
	for _, s := range benchIndexSizes {
		b.Run(s.name, func(b *testing.B) {
			data, err := EncodeIndex(benchIndex(s.entries, s.refs))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, err := ParseIndex(data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkJSONIndexMarshal(b *testing.B) {
	for _, s := range benchIndexSizes {
		b.Run(s.name, func(b *testing.B) {
			ix := benchIndex(s.entries, s.refs)
			data, err := EncodeIndex(ix)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, err := EncodeIndex(ix); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchEntry(refs int) (Header, []RefUpdate) {
	h := Header{
		V: 1, Kind: KindPush, Seq: 1043, At: benchAt, Subject: "https://auth.latere.ai|0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f",
		PackBytes: 1 << 20, PackSHA256: benchHex(1, 64), PushOptions: []string{"origo.event=off"},
	}
	updates := make([]RefUpdate, 0, refs)
	for i := range refs {
		updates = append(updates, RefUpdate{Ref: fmt.Sprintf("refs/heads/feature/topic-%04d", i), Old: benchHex(i, 40), New: benchHex(i+1, 40)})
	}
	return h, updates
}

func BenchmarkJSONEntryHeadMarshal(b *testing.B) {
	for _, s := range []struct {
		name string
		refs int
	}{{"small", 1}, {"medium", 60}, {"large", 1300}} {
		b.Run(s.name, func(b *testing.B) {
			h, refs := benchEntry(s.refs)
			data, err := EncodeEntryHead(h, refs)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, err := EncodeEntryHead(h, refs); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONEntryHeadUnmarshal reads the two lines of an entry the
// way a replay or a repair sweep does.
func BenchmarkJSONEntryHeadUnmarshal(b *testing.B) {
	for _, s := range []struct {
		name string
		refs int
	}{{"small", 1}, {"medium", 60}, {"large", 1300}} {
		b.Run(s.name, func(b *testing.B) {
			h, refs := benchEntry(s.refs)
			hb, err := EncodeEntryHead(h, refs)
			if err != nil {
				b.Fatal(err)
			}
			nl := strings.IndexByte(string(hb), '\n')
			head, tx := hb[:nl], hb[nl+1:len(hb)-1]
			b.ReportAllocs()
			b.SetBytes(int64(len(hb)))
			for b.Loop() {
				if _, err := ParseHeader(head); err != nil {
					b.Fatal(err)
				}
				if _, err := ParseTransaction(tx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONMetaRoundTrip writes and reads a repository's metadata
// object through the log, over the in-memory store.
func BenchmarkJSONMetaRoundTrip(b *testing.B) {
	ctx := context.Background()
	l := New(Options{Store: NewMemStore(), Now: func() time.Time { return benchAt }, Logger: slog.New(slog.DiscardHandler)})
	frozen, verified, equal := benchAt, benchAt.Add(time.Hour), true
	m := &Meta{
		ID: "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f", Owner: "acme", Slug: "billing-service",
		Creator: "https://auth.latere.ai|1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d", CreatedAt: benchAt, UpdatedAt: benchAt,
		FrozenAt: &frozen, VerifiedAt: &verified, VerifiedEqual: &equal,
	}
	if err := l.WriteMeta(ctx, m); err != nil {
		b.Fatal(err)
	}
	obj, err := l.store.Head(ctx, l.metaKey(m.ID))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(obj.Size)
	for b.Loop() {
		if err := l.WriteMeta(ctx, m); err != nil {
			b.Fatal(err)
		}
		if _, err := l.ReadMeta(ctx, m.ID); err != nil {
			b.Fatal(err)
		}
	}
}
