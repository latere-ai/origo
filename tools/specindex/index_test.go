// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSpecs(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const front = "---\ntitle: t\nstatus: drafted\n---\n\n"

func TestDefinitionsMentionsAndFindings(t *testing.T) {
	dir := writeSpecs(t, map[string]string{
		"001-a.md": front + "| Code | Status | Message |\n|---|---|---|\n| `repo_not_found` | 404 | Repository not found. |\n\n" +
			"| Variable | Default |\n|---|---|\n| `ORIGO_S3_BUCKET`, `ORIGO_S3_KEY` | none |\n\n" +
			"| Method | Path | Result |\n|---|---|---|\n| POST | `/v1/repos` | 201 |\n| GET | `/v1/repos/{id}` | `repo_not_found` on a missing id |\n\n" +
			"| Header | Meaning |\n|---|---|\n| `Origo-Contract` | version |\n\n" +
			"| Metric | Type |\n|---|---|\n| `origo_pushes_total` | counter |\n\n" +
			"| Event | Payload |\n|---|---|\n| `push` | the push |\n\n" +
			"| Failpoint | Reached |\n|---|---|\n| `commit.before-index` | before the index create |\n\n" +
			"A cell with a pipe: `\"push\"\\|\"compact\"` is not a table kind.\n",
		"002-b.md": front + "Uses `ORIGO_S3_BUCKET`, `origo_pushes_total{result=\"ok\"}`, `Origo-Contract`, `POST /v1/repos`, `repo_not_found`, and the `push` event.\n" +
			"In one pair: `Origo-Event: push`, `ORIGO_FAILPOINT=commit.before-index`, and `Origo-Contract: 1`.\n" +
			"```\n`ORIGO_IN_A_FENCE` is not a mention\n```\n" +
			"Names nothing defines: `ORIGO_NOWHERE`, `origo_nowhere_total`, `Origo-Nowhere`, `GET /nowhere`.\n" +
			"| Code | Status | Message |\n|---|---|---|\n| `repo_not_found` | 404 | duplicate |\n",
		"README.md": "not a spec",
	})
	idx, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"error code\x00repo_not_found": "001", "variable\x00ORIGO_S3_BUCKET": "001", "variable\x00ORIGO_S3_KEY": "001",
		"endpoint\x00POST /v1/repos": "001", "endpoint\x00GET /v1/repos/{id}": "001", "header\x00Origo-Contract": "001",
		"metric\x00origo_pushes_total": "001", "event\x00push": "001", "failpoint\x00commit.before-index": "001",
	}
	got := map[string]string{}
	for _, n := range idx.Names {
		got[string(n.Kind)+"\x00"+n.Name] = n.Owner
		switch n.Name {
		case "ORIGO_S3_BUCKET", "origo_pushes_total", "Origo-Contract", "POST /v1/repos", "repo_not_found", "push", "commit.before-index", "ORIGO_FAILPOINT":
			if strings.Join(n.Also, ",") != "002" {
				t.Errorf("%s: also %v, want 002", n.Name, n.Also)
			}
		default:
			if len(n.Also) != 0 {
				t.Errorf("%s: also %v, want none", n.Name, n.Also)
			}
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q owner %q, want %q", strings.ReplaceAll(k, "\x00", " "), got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("names %v", got)
	}
	wantFindings := []string{
		`002-b.md: endpoint "GET /nowhere" is named but no spec defines it`,
		`002-b.md: header "Origo-Event" is named but no spec defines it`,
		`002-b.md: header "Origo-Nowhere" is named but no spec defines it`,
		`002-b.md: metric "origo_nowhere_total" is named but no spec defines it`,
		`002-b.md: variable "ORIGO_FAILPOINT" is named but no spec defines it`,
		`002-b.md: variable "ORIGO_NOWHERE" is named but no spec defines it`,
		`error code "repo_not_found" is defined by 001 and by 002`,
	}
	if strings.Join(idx.Findings, "\n") != strings.Join(wantFindings, "\n") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(idx.Findings, "\n"), strings.Join(wantFindings, "\n"))
	}
}

func TestSpliceAndCurrentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# Specs\n\n"+BeginMarker+"\nold\n"+EndMarker+"\ntail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Splice(readme, "| a |\n"); err != nil {
		t.Fatal(err)
	}
	cur, err := Current(readme)
	if err != nil || cur != "| a |\n" {
		t.Fatalf("current %q, %v", cur, err)
	}
	data, _ := os.ReadFile(readme)
	if !strings.HasSuffix(string(data), EndMarker+"\ntail\n") {
		t.Fatalf("tail lost: %q", data)
	}
	if err := os.WriteFile(readme, []byte("no markers"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Current(readme); err == nil {
		t.Fatal("missing markers not reported")
	}
	if err := Splice(readme, "x"); err == nil {
		t.Fatal("missing markers not reported by Splice")
	}
}

// TestReadmeTableIsCurrent is the drift check: the deck has no findings
// and the table in specs/README.md equals what the specs define.
func TestReadmeTableIsCurrent(t *testing.T) {
	idx, err := Build("../../specs")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range idx.Findings {
		t.Error(f)
	}
	current, err := Current("../../specs/README.md")
	if err != nil {
		t.Fatal(err)
	}
	if current != idx.Table() {
		t.Error("specs/README.md cross-reference table differs from the specs; run: cd tools/specindex && go run . -write")
	}
}
