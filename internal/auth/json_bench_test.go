// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/json"
	"fmt"
	"testing"
)

// BenchmarkJSONDirectoryUnmarshal is the authorizer's answer to a list
// call (spec 026): the page of repositories the subject may see, parsed
// by the client on every GET /v1/repos.
func BenchmarkJSONDirectoryUnmarshal(b *testing.B) {
	for _, s := range []struct {
		name string
		n    int
	}{{"small", 4}, {"medium", 100}, {"large", 2200}} {
		b.Run(s.name, func(b *testing.B) {
			repos := make([]DirectoryEntry, 0, s.n)
			for i := 0; i < s.n; i++ {
				repos = append(repos, DirectoryEntry{ID: fmt.Sprintf("0f5c1d2e-3a4b-4c5d-8e6f-%012d", i), Owner: "acme", Slug: fmt.Sprintf("service-%04d", i)})
			}
			raw, err := json.Marshal(map[string]any{"repos": repos, "next_cursor": "0f5c1d2e-3a4b-4c5d-8e6f-ffffffffffff"})
			if err != nil {
				b.Fatal(err)
			}
			c := &Client{}
			if dir, err := c.directory(raw); err != nil || len(dir.Repos) != s.n {
				b.Fatalf("%d repos: %v", len(dir.Repos), err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				if _, err := c.directory(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
