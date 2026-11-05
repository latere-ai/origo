// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"latere.ai/x/origo/internal/wal"
)

var benchNow = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

const benchRepo = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

// benchDispatcher is a dispatcher over an in-memory store with events
// off: write, read, and the cursor never touch the network.
func benchDispatcher(b *testing.B) (*Dispatcher, *wal.MemStore) {
	b.Helper()
	store := wal.NewMemStore()
	logger := slog.New(slog.DiscardHandler)
	l := wal.New(wal.Options{Store: store, Now: func() time.Time { return benchNow }, Logger: logger})
	d, err := New(Options{Log: l, Node: "origo-0", Now: func() time.Time { return benchNow }, Logger: logger})
	if err != nil {
		b.Fatal(err)
	}
	return d, store
}

func benchSHA(n int) string { return fmt.Sprintf("%040x", n*7919+13) }

func benchPush(updates int) Push {
	p := Push{
		ID: "6f1c2d3e-4a5b-5c6d-8e7f-0a1b2c3d4e5f", Kind: KindPush, Repo: benchRepo, Seq: 1043,
		Owner: "acme", Slug: "billing-service", Pusher: Pusher{Sub: "https://auth.latere.ai|1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"},
		At: benchNow, Updates: make([]Update, 0, updates),
	}
	for i := range updates {
		p.Updates = append(p.Updates, Update{Ref: fmt.Sprintf("refs/heads/feature/topic-%04d", i), Before: benchSHA(i), After: benchSHA(i + 1), Forced: i%9 == 0})
	}
	return p
}

var benchPushSizes = []struct {
	name    string
	updates int
}{{"small", 2}, {"medium", 50}, {"large", 1100}}

// BenchmarkJSONPushEventMarshal is the enqueue after a push: the
// payload, then the object that wraps it, then the store write.
func BenchmarkJSONPushEventMarshal(b *testing.B) {
	for _, s := range benchPushSizes {
		b.Run(s.name, func(b *testing.B) {
			ctx := context.Background()
			d, store := benchDispatcher(b)
			p := benchPush(s.updates)
			key := d.pushKey(benchRepo, p.Seq)
			if err := d.write(ctx, key, p, benchNow, false); err != nil {
				b.Fatal(err)
			}
			obj, err := store.Head(ctx, key)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(obj.Size)
			for b.Loop() {
				if err := d.write(ctx, key, p, benchNow, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONPushEventUnmarshal is the dispatcher reading a pending
// object back: the wrapper, then the envelope inside it.
func BenchmarkJSONPushEventUnmarshal(b *testing.B) {
	for _, s := range benchPushSizes {
		b.Run(s.name, func(b *testing.B) {
			ctx := context.Background()
			d, store := benchDispatcher(b)
			p := benchPush(s.updates)
			key := d.pushKey(benchRepo, p.Seq)
			if err := d.write(ctx, key, p, benchNow, false); err != nil {
				b.Fatal(err)
			}
			obj, err := store.Head(ctx, key)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(obj.Size)
			for b.Loop() {
				if _, _, _, err := d.read(ctx, key); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONCursorRoundTrip is one delivery's cursor advance: read
// the repository's cursor, add the delivery, write it back.
func BenchmarkJSONCursorRoundTrip(b *testing.B) {
	for _, s := range []struct {
		name      string
		delivered int
	}{{"small", 5}, {"medium", 150}, {"large", 3000}} {
		b.Run(s.name, func(b *testing.B) {
			ctx := context.Background()
			d, store := benchDispatcher(b)
			c := cursor{Seq: 1043, Delivered: make([]delivered, 0, s.delivered)}
			for i := 0; i < s.delivered; i++ {
				c.Delivered = append(c.Delivered, delivered{ID: fmt.Sprintf("6f1c2d3e-4a5b-5c6d-8e7f-%012d", i), At: benchNow.Add(-time.Duration(i+1) * time.Second)})
			}
			// A push raises the sequence only, so the stored cursor is
			// the same size on every iteration.
			seed, err := json.Marshal(c, json.Deterministic(true))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := store.Put(ctx, d.cursorKey(benchRepo), wal.BytesBody(seed)); err != nil {
				b.Fatal(err)
			}
			env := envelope{ID: "6f1c2d3e-4a5b-5c6d-8e7f-ffffffffffff", Kind: KindPush, Repo: benchRepo, Seq: 1043, At: benchNow}
			b.ReportAllocs()
			b.SetBytes(int64(len(seed)))
			for b.Loop() {
				if err := d.advanceCursor(ctx, env); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
