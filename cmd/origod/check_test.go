// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/s3/s3test"

	"github.com/latere-ai/origo/test/stubs/sink"
)

// checkEnv is a configuration every requirement can meet: the fake
// bucket, which honours the conditional create, the stub issuer and
// authorizer of the identity, a stub sink, and a writable directory.
func checkEnv(t *testing.T) map[string]string {
	t.Helper()
	env, _ := newEnv(t)
	bucket := s3test.New(t, "origo")
	env["ORIGO_S3_ENDPOINT"] = bucket.URL()
	env["ORIGO_S3_REGION"] = s3test.Region
	env["ORIGO_S3_KEY"], env["ORIGO_S3_SECRET"] = s3test.Key, s3test.Secret
	env["ORIGO_S3_PATH_STYLE"] = "1"
	env["ORIGO_EVENTS_URL"] = sink.New(t).URL()
	env["ORIGO_EVENTS_SECRET"] = sink.DefaultSecret
	return env
}

// report runs the check and returns its exit code and its lines.
func report(t *testing.T, env map[string]string) (int, []string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"check"}, getenv(env), &out, &errOut)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("%d lines, want 7: %q %q", len(lines), out.String(), errOut.String())
	}
	return code, lines
}

// requires asserts that the named requirement failed with a detail and
// that every other line passed, which is what makes each case below a
// statement about one requirement rather than about the whole report.
func requires(t *testing.T, lines []string, name, detail string) {
	t.Helper()
	want := "fail " + name + ": "
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, want):
			if !strings.Contains(l, detail) {
				t.Errorf("%q does not name %q", l, detail)
			}
		case strings.HasPrefix(l, "fail "):
			t.Errorf("a second requirement failed: %q", l)
		}
	}
	if !strings.Contains(strings.Join(lines, "\n"), want) {
		t.Errorf("no line reads %q:\n%s", want, strings.Join(lines, "\n"))
	}
}

// TestCheckReportsEachRequirement is spec 018's criterion: every
// requirement fails on its own and names itself, the report is seven
// lines either way, and a sound installation prints seven ok lines and
// exits 0.
func TestCheckReportsEachRequirement(t *testing.T) {
	t.Run("everything in place", func(t *testing.T) {
		code, lines := report(t, checkEnv(t))
		for _, l := range lines {
			if !strings.HasPrefix(l, "ok ") {
				t.Errorf("not ok: %q", l)
			}
		}
		if code != 0 {
			t.Errorf("exit %d with everything in place", code)
		}
	})

	t.Run("an unreachable bucket", func(t *testing.T) {
		env := checkEnv(t)
		env["ORIGO_S3_ENDPOINT"] = "http://127.0.0.1:1"
		code, lines := report(t, env)
		// The conditional create runs against the same endpoint, so it
		// fails beside the listing; the line naming the bucket is the
		// one an operator reads first.
		if code != 1 || !strings.HasPrefix(lines[0], "fail bucket: ") {
			t.Fatalf("exit %d: %q", code, lines)
		}
	})

	t.Run("a store that ignores the conditional create", func(t *testing.T) {
		env := checkEnv(t)
		env["ORIGO_CHECK_SELFTEST"] = "1"
		code, lines := report(t, env)
		requires(t, lines, "conditional-create", "second create answered 200")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("an unreachable issuer", func(t *testing.T) {
		env := checkEnv(t)
		env["ORIGO_OIDC_ISSUERS"] = "http://127.0.0.1:1"
		env["ORIGO_OIDC_INSECURE_ISSUERS"] = "http://127.0.0.1:1"
		code, lines := report(t, env)
		requires(t, lines, "issuer", "127.0.0.1:1")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("an authorizer that does not answer", func(t *testing.T) {
		env := checkEnv(t)
		env["ORIGO_AUTHORIZER_URL"] = answering(t, http.StatusInternalServerError, "")
		code, lines := report(t, env)
		requires(t, lines, "authorizer", "500")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("an authorizer that allows the probe id", func(t *testing.T) {
		env := checkEnv(t)
		env["ORIGO_AUTHORIZER_URL"] = answering(t, http.StatusOK, `{"allow":true}`)
		code, lines := report(t, env)
		requires(t, lines, "authorizer", "probe id allowed")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("a sink that refuses the ping", func(t *testing.T) {
		env := checkEnv(t)
		env["ORIGO_EVENTS_URL"] = answering(t, http.StatusBadGateway, "")
		code, lines := report(t, env)
		requires(t, lines, "events", "502")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("an unwritable data directory", func(t *testing.T) {
		env := checkEnv(t)
		file := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		env["ORIGO_DATA_DIR"] = filepath.Join(file, "data")
		code, lines := report(t, env)
		requires(t, lines, "disk", "ORIGO_DATA_DIR")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("a cache larger than the disk", func(t *testing.T) {
		env := checkEnv(t)
		env["ORIGO_CACHE_BYTES"] = "9223372036854775807"
		code, lines := report(t, env)
		requires(t, lines, "disk", "ORIGO_CACHE_BYTES")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("no git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		code, lines := report(t, checkEnv(t))
		requires(t, lines, "git", "executable file not found")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("a git below the floor", func(t *testing.T) {
		t.Setenv("PATH", stubGit(t, "git version 2.39.5"))
		code, lines := report(t, checkEnv(t))
		requires(t, lines, "git", "2.39.5 is below 2.40")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})

	t.Run("a git that answers nothing usable", func(t *testing.T) {
		t.Setenv("PATH", stubGit(t, "not a version"))
		code, lines := report(t, checkEnv(t))
		requires(t, lines, "git", "git --version answered")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
	})
}

// answering starts a server that answers every request the same way and
// returns its URL.
func answering(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// stubGit writes a git on a PATH of its own that prints one line.
func stubGit(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + version + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGitVersionParsesTheThreeNumbers holds the parse itself: the word
// after the second one, its first three dot-separated numbers, and a
// trailing word a distribution appends ignored.
func TestGitVersionParsesTheThreeNumbers(t *testing.T) {
	for _, c := range []struct {
		out  string
		want [3]int
		bad  bool
	}{
		{out: "git version 2.47.1\n", want: [3]int{2, 47, 1}},
		{out: "git version 2.51.0.windows.1\n", want: [3]int{2, 51, 0}},
		{out: "git version 2.39\n", want: [3]int{2, 39, 0}},
		{out: "git version\n", bad: true},
		{out: "git version two\n", bad: true},
	} {
		_, got, err := gitVersion(c.out)
		if c.bad {
			if err == nil {
				t.Errorf("%q parsed as %v", c.out, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: %v %v, want %v", c.out, got, err, c.want)
		}
	}
}

// TestSubcommandDispatch is spec 002's table, which spec 018 completes:
// check runs the check, the bare binary and serve both serve, -version
// answers under every name, and an unknown word is a usage error.
func TestSubcommandDispatch(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"check"}, getenv(checkEnv(t)), &out, &errOut); code != 0 ||
		!strings.HasPrefix(out.String(), "ok bucket\n") {
		t.Fatalf("check: %d %q %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"check", "-version"}, getenv(nil), &out, &errOut); code != 0 ||
		!strings.HasPrefix(out.String(), "origod ") {
		t.Fatalf("check -version: %d %q", code, out.String())
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"check", "-badflag"}, getenv(nil), &out, &errOut); code != 2 {
		t.Fatalf("check with an unknown flag: %d", code)
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"check"}, getenv(nil), &out, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "configuration: missing ORIGO_AUTHORIZER_TOKEN") {
		t.Fatalf("check without a configuration: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"nosuch"}, getenv(nil), &out, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), `unknown subcommand "nosuch"; serve (the default), check, and migrate`) {
		t.Fatalf("unknown subcommand: %d %q", code, errOut.String())
	}
	out.Reset()
	if code := run(context.Background(), []string{"-version"}, getenv(nil), &out, &errOut); code != 0 ||
		!strings.HasPrefix(out.String(), "origod ") {
		t.Fatalf("-version: %d %q", code, out.String())
	}

	// The bare binary and serve are the same node: each starts, serves,
	// and stops when the context ends.
	for _, args := range [][]string{nil, {"serve"}} {
		ctx, cancel := context.WithCancel(context.Background())
		env := testEnv(t)
		var out, errOut syncBuffer
		done := make(chan int, 1)
		go func() { done <- run(ctx, args, getenv(env), &out, &errOut) }()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(out.String(), "serving") {
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("%v did not serve: %s %s", args, out.String(), errOut.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if code := <-done; code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut.String())
		}
	}
}
