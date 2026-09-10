// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package origocli_test

import (
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/origocli"
)

// TestStartupRefusals: each one is a sentence on stderr and exit 2, and no
// request leaves the process, so a mistyped variable costs no round trip.
func TestStartupRefusals(t *testing.T) {
	f := newFake()
	base := envFor(f.start(t))
	cases := []struct {
		name    string
		change  map[string]string
		args    []string
		carries string
	}{
		{"no url", map[string]string{"ORIGO_URL": ""}, []string{"info"}, "ORIGO_URL"},
		{"not a url", map[string]string{"ORIGO_URL": "git.example.com"}, []string{"info"}, "absolute http or https"},
		{"plain http off the loopback", map[string]string{"ORIGO_URL": "http://git.example.com"}, []string{"info"}, "https, except at a loopback"},
		{"no token", map[string]string{"ORIGO_TOKEN": ""}, []string{"info"}, "ORIGO_TOKEN"},
		{"a signing key as a bearer", map[string]string{"ORIGO_TOKEN": "-----BEGIN EC PRIVATE KEY-----\nx\n"}, []string{"info"}, "ORIGO_TOKEN_KEY"},
		{"no author on a write", map[string]string{"ORIGO_AUTHOR": ""}, []string{"commit", "-m", "x", "-expect", head, "a.txt"}, "ORIGO_AUTHOR"},
		{"an author with no address", map[string]string{"ORIGO_AUTHOR": "Ada Lovelace"}, []string{"commit", "-m", "x", "-expect", head, "a.txt"}, `"Name <email>"`},
		{"an address with no at sign", map[string]string{"ORIGO_AUTHOR": "Ada <ada.example.com>"}, []string{"commit", "-m", "x", "-expect", head, "a.txt"}, "holds an @"},
		{"an author with no name", map[string]string{"ORIGO_AUTHOR": "<ada@example.com>"}, []string{"commit", "-m", "x", "-expect", head, "a.txt"}, "holds an @"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := maps.Clone(base)
			maps.Copy(env, tc.change)
			before := len(f.seen())
			got := run(t, env, tc.args...)
			if got.code != origocli.CodeMisusage {
				t.Fatalf("exit %d, want 2", got.code)
			}
			if !strings.Contains(got.stderr, tc.carries) {
				t.Fatalf("%q does not carry %q", got.stderr, tc.carries)
			}
			if len(strings.Split(strings.TrimSpace(got.stderr), "\n")) != 1 {
				t.Fatalf("more than one sentence: %q", got.stderr)
			}
			if len(f.seen()) != before {
				t.Fatal("a request left the process")
			}
			if got.stdout != "" {
				t.Fatalf("a refusal wrote to stdout: %q", got.stdout)
			}
		})
	}
	// A loopback address is served over plain http, which is what a local
	// stack and every test in this file run on.
	for _, host := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		env := maps.Clone(base)
		env["ORIGO_URL"] = host
		if got := run(t, env, "info"); got.code == origocli.CodeMisusage {
			t.Fatalf("%s was refused at start-up: %q", host, got.stderr)
		}
	}
	// A write command with no author is refused, and a read one is not: the
	// variable is only required by what needs it.
	env := maps.Clone(base)
	env["ORIGO_AUTHOR"] = ""
	if got := run(t, env, "info"); got.code != origocli.CodeOK {
		t.Fatalf("a read needed ORIGO_AUTHOR: %d %q", got.code, got.stderr)
	}
}

// TestOrigoErrorsBecomeOneLine walks the error table of spec 025.
func TestOrigoErrorsBecomeOneLine(t *testing.T) {
	cases := []struct {
		status  int
		code    string
		details map[string]any
		headers map[string]string
		want    []string
		absent  []string
	}{
		{409, contract.CodeNonFastForward, map[string]any{"expected": "aaa", "actual": "bbb", "ref": "refs/heads/main"}, nil,
			[]string{"non_fast_forward:", "expected=aaa", "actual=bbb"}, []string{"ref="}},
		{409, contract.CodeMergeConflict, map[string]any{"commit": "c1", "paths": []any{"b.go", "a.go"}}, nil,
			[]string{"merge_conflict:", "commit=c1", "paths=a.go,b.go"}, nil},
		{400, contract.CodeInvalidChange, map[string]any{"index": float64(3), "reason": "path"}, nil,
			[]string{"invalid_change:", "index=3", "reason=path"}, nil},
		{413, contract.CodeOverQuota, map[string]any{"limit": "repository", "bytes": float64(10), "max": float64(5)}, nil,
			[]string{"over_quota:", "limit=repository", "bytes=10", "max=5"}, nil},
		{404, contract.CodeRefNotFound, map[string]any{"ref": "topic"}, nil, []string{"ref_not_found:", "ref=topic"}, nil},
		{404, contract.CodeRepoNotFound, map[string]any{"id": "id-9"}, nil, []string{"repo_not_found:"}, []string{"id="}},
		{403, contract.CodeRepoFrozen, map[string]any{"frozen_at": "now"}, nil, []string{"repo_frozen:"}, []string{"frozen_at="}},
		{410, contract.CodeGone, map[string]any{"id": "id-9"}, nil, []string{"gone:"}, []string{"id="}},
		{401, contract.CodeUnauthenticated, map[string]any{"reason": "expired"}, nil,
			[]string{"unauthenticated:", "reason=expired", "mint a fresh token"}, nil},
		{401, contract.CodeUnauthenticated, map[string]any{"reason": "signature"}, nil,
			[]string{"unauthenticated:", "reason=signature"}, []string{"mint a fresh token"}},
		{403, contract.CodeForbidden, map[string]any{"action": "write", "reason": "not a member"}, nil,
			[]string{"forbidden:", "action=write", "reason=not a member"}, nil},
		{429, contract.CodeRateLimited, map[string]any{"limit": "repository"}, map[string]string{"Retry-After": "60", "RateLimit-Limit": "120"},
			[]string{"rate_limited:", "retry_after=60", "rate_limit=120"}, nil},
		{503, contract.CodeStorageUnavailable, nil, map[string]string{"Retry-After": "30"},
			[]string{"storage_unavailable:", "retry_after=30"}, []string{"rate_limit="}},
		{503, contract.CodeAuthorizerUnavailable, map[string]any{"retry_after": float64(15)}, nil,
			[]string{"authorizer_unavailable:", "retry_after=15"}, nil},
		{503, contract.CodeRepositoryUnavailable, nil, nil, []string{"repository_unavailable:"}, nil},
		{504, contract.CodeOperationTimeout, map[string]any{"operation": "merge", "budget_seconds": float64(300)}, nil,
			[]string{"operation_timeout:", "operation=merge", "budget_seconds=300"}, nil},
		{400, contract.CodeInvalid, map[string]any{"reason": "limit", "field": "limit"}, nil,
			[]string{"invalid_request:", "reason=limit", "field=limit"}, nil},
		{413, contract.CodeBlobTooLarge, map[string]any{"size": float64(1), "max": float64(2)}, nil,
			[]string{"blob_too_large:", "size=1", "max=2"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.code+"/"+strings.Join(tc.want[:1], ""), func(t *testing.T) {
			f := newFake()
			f.answer = func(w http.ResponseWriter, r *http.Request) bool {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				f.refuse(w, tc.status, tc.code, tc.details)
				return true
			}
			got := run(t, envFor(f.start(t)), "info")
			if got.code != origocli.CodeRefused {
				t.Fatalf("exit %d, want 1", got.code)
			}
			line := strings.TrimSpace(got.stderr)
			if strings.Contains(line, "\n") {
				t.Fatalf("more than one line: %q", got.stderr)
			}
			if !strings.HasPrefix(line, tc.code+": "+sentence(t, tc.code)) {
				t.Fatalf("the line does not open with the code and its sentence: %q", line)
			}
			for _, want := range tc.want {
				if !strings.Contains(line, want) {
					t.Fatalf("%q missing from %q", want, line)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(line, absent) {
					t.Fatalf("%q reached the line %q", absent, line)
				}
			}
			if got.stdout != "" {
				t.Fatalf("a refusal wrote to stdout: %q", got.stdout)
			}
		})
	}
}

func TestAnUnexpectedBodyIsOneLineAndNeverTheBody(t *testing.T) {
	f := newFake()
	f.answer = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html>a proxy said something long and useless</html>"))
		return true
	}
	got := run(t, envFor(f.start(t)), "info")
	if got.code != origocli.CodeRefused {
		t.Fatalf("exit %d", got.code)
	}
	if strings.Contains(got.stderr, "<html>") {
		t.Fatalf("the body reached the reader: %q", got.stderr)
	}
	if !strings.Contains(got.stderr, "502") {
		t.Fatalf("the status is not named: %q", got.stderr)
	}
}

func TestAnUnreachableInstallationIsOneLine(t *testing.T) {
	env := envFor("http://127.0.0.1:1")
	got := run(t, env, "info")
	if got.code != origocli.CodeRefused {
		t.Fatalf("exit %d", got.code)
	}
	if !strings.HasPrefix(got.stderr, "origo: ") {
		t.Fatalf("stderr: %q", got.stderr)
	}
}

// TestDispatchAndExitCodes covers the surface's edges: the help page, an
// unknown command, and the version.
func TestDispatchAndExitCodes(t *testing.T) {
	f := newFake()
	env := envFor(f.start(t))

	if got := run(t, env); got.code != origocli.CodeMisusage || !strings.Contains(got.stdout, "usage: origo") {
		t.Fatalf("no argument: %d %q", got.code, got.stdout)
	}
	for _, arg := range []string{"help", "-h", "--help", "-help"} {
		got := run(t, env, arg)
		if got.code != origocli.CodeOK {
			t.Fatalf("%s exited %d", arg, got.code)
		}
		if !strings.Contains(got.stdout, "ORIGO_URL") || !strings.Contains(got.stdout, "grep -i handler") {
			t.Fatalf("%s printed no help:\n%s", arg, got.stdout)
		}
	}
	if got := run(t, env, "version"); got.code != origocli.CodeOK || got.stdout == "" {
		t.Fatalf("version: %d %q", got.code, got.stdout)
	}
	got := run(t, env, "clone")
	if got.code != origocli.CodeMisusage || !strings.Contains(got.stderr, "unknown command") {
		t.Fatalf("an unknown command: %d %q", got.code, got.stderr)
	}
	// A bad flag is a usage error, not a refusal, and nothing is requested.
	before := len(f.seen())
	if got := run(t, env, "log", "-nope"); got.code != origocli.CodeMisusage {
		t.Fatalf("a bad flag exited %d", got.code)
	}
	if len(f.seen()) != before {
		t.Fatal("a bad flag reached the wire")
	}
}

func TestNoRepositoryNamedIsAUsageError(t *testing.T) {
	f := newFake()
	env := envFor(f.start(t))
	env["ORIGO_REPO"] = ""
	for _, args := range [][]string{{"info"}, {"refs"}, {"ls"}, {"cat", "a.go"}, {"log"}, {"show", head}, {"diff", "a", "b"}} {
		got := run(t, env, args...)
		if got.code != origocli.CodeMisusage {
			t.Fatalf("%v exited %d", args, got.code)
		}
		if !strings.Contains(got.stderr, "-repo") {
			t.Fatalf("%v: %q", args, got.stderr)
		}
	}
}

func TestReadMisuseIsRefusedBeforeAnyRequest(t *testing.T) {
	f := newFake()
	env := envFor(f.start(t))
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"ls", "a", "b"}, "at most one path"},
		{[]string{"cat"}, "cat takes one path"},
		{[]string{"cat", "-offset", "-1", "a.go"}, "-offset counts from 0"},
		{[]string{"show"}, "show takes one commit"},
		{[]string{"diff", "a"}, "a base and a head"},
	}
	before := len(f.seen())
	for _, tc := range cases {
		got := run(t, env, tc.args...)
		if got.code != origocli.CodeMisusage {
			t.Fatalf("%v exited %d", tc.args, got.code)
		}
		if !strings.Contains(got.stderr, tc.want) {
			t.Fatalf("%v: %q does not carry %q", tc.args, got.stderr, tc.want)
		}
	}
	if len(f.seen()) != before {
		t.Fatal("a misuse reached the wire")
	}
}

func TestCatRefusesADirectory(t *testing.T) {
	f := newFake()
	f.entries = []map[string]any{entryJSON("internal", "040000", "tree", 0)}
	got := run(t, envFor(f.start(t)), "cat", "internal")
	if got.code != origocli.CodeMisusage || !strings.Contains(got.stderr, "origo ls") {
		t.Fatalf("%d %q", got.code, got.stderr)
	}
}

// TestARefWithASlashReachesEveryReadCommand is fact 1 seen from the surface:
// a branch named feature/x works everywhere, because the name never travels
// in a path segment.
func TestARefWithASlashReachesEveryReadCommand(t *testing.T) {
	f := newFake()
	f.entries = []map[string]any{entryJSON("a.go", "100644", "blob", 4)}
	f.blobs["sha-a.go"] = []byte("one\n")
	f.commits = []map[string]any{commitJSON(head, "Ada", "2026-09-10T12:00:00Z", "a change")}
	f.diff = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@\n+one\n"
	env := envFor(f.start(t))
	for _, args := range [][]string{
		{"ls", "-ref", "feature/x"},
		{"cat", "-ref", "feature/x", "a.go"},
		{"log", "-ref", "feature/x"},
		{"diff", "feature/x", "main"},
	} {
		got := run(t, env, args...)
		if got.code != origocli.CodeOK {
			t.Fatalf("%v exited %d: %s", args, got.code, got.stderr)
		}
	}
	for _, c := range f.seen() {
		path, _, _ := strings.Cut(strings.TrimPrefix(c, "GET "), "?")
		if strings.Contains(path, "feature") {
			t.Fatalf("a reference reached a path segment: %s", c)
		}
	}
}
