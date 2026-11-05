// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package placement

import (
	"context"
	"log/slog"
	"testing"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"

	"latere.ai/x/origo/internal/metrics"
)

// benchHolder holds every repository at a sequence past any
// announcement, so Handle answers without scheduling a catch-up.
type benchHolder struct{}

func (benchHolder) Held(string) (uint64, bool)            { return 1 << 40, true }
func (benchHolder) CatchUp(context.Context, string) error { return nil }

// BenchmarkJSONGossipDatagramRoundTrip is the inter-node datagram: Seal
// encodes and signs an announcement, Handle verifies and decodes it.
// Every push sends three per peer, and every heartbeat one.
func BenchmarkJSONGossipDatagramRoundTrip(b *testing.B) {
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	g, err := NewGossip(GossipOptions{
		Set: NewSet("origo-0", func() time.Time { return at }), Secret: secret, Peers: "127.0.0.1:1", Holder: benchHolder{},
		Now: func() time.Time { return at }, Logger: slog.New(slog.DiscardHandler), Metrics: metrics.Register(pkgmetrics.NewRegistry()),
	})
	if err != nil {
		b.Fatal(err)
	}
	p := payload{V: version, Node: "origo-1", Repo: repoA, Seq: 1043, At: at}
	probe, err := Seal(secret, p)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	if err := g.Handle(ctx, probe); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(probe)))
	for b.Loop() {
		d, err := Seal(secret, p)
		if err != nil {
			b.Fatal(err)
		}
		if err := g.Handle(ctx, d); err != nil {
			b.Fatal(err)
		}
	}
}
