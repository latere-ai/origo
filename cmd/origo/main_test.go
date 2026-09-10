// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/origocli"
)

// TestMainDispatchesAndExits: main holds wiring only, so what this file proves
// is that the wiring is the whole of it. The behaviour is tested in
// internal/origocli, which a test reaches without a process.
func TestMainDispatchesAndExits(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := func(string) string { return "" }
	if code := origocli.Run(context.Background(), []string{"help"}, env, &stdout, &stderr); code != origocli.CodeOK {
		t.Fatalf("help exited %d", code)
	}
	if !strings.Contains(stdout.String(), "usage: origo") {
		t.Fatalf("stdout: %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := origocli.Run(context.Background(), []string{"clone"}, env, &stdout, &stderr); code != origocli.CodeMisusage {
		t.Fatalf("an unknown command exited %d", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := origocli.Run(context.Background(), []string{"info"}, env, &stdout, &stderr); code != origocli.CodeMisusage {
		t.Fatalf("a missing environment exited %d", code)
	}
}

// TestTheBinaryCarriesTheExitCodesThroughTheProcess is the one thing a test of
// Run cannot see: os.Exit carries the code a shell reads, which is what makes
// `origo ... || handle` work in a pipeline.
func TestTheBinaryCarriesTheExitCodesThroughTheProcess(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "origo")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cases := []struct {
		args []string
		env  []string
		want int
	}{
		{[]string{"help"}, nil, origocli.CodeOK},
		{[]string{"version"}, nil, origocli.CodeOK},
		{[]string{"clone"}, nil, origocli.CodeMisusage},
		{[]string{"info"}, nil, origocli.CodeMisusage},
		{[]string{"info"}, []string{"ORIGO_URL=http://127.0.0.1:1", "ORIGO_TOKEN=t", "ORIGO_REPO=r"}, origocli.CodeRefused},
	}
	for _, tc := range cases {
		cmd := exec.CommandContext(t.Context(), bin, tc.args...)
		cmd.Env = tc.env
		err := cmd.Run()
		got := 0
		var exit *exec.ExitError
		if errorsAs(err, &exit) {
			got = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got != tc.want {
			t.Fatalf("%v exited %d, want %d", tc.args, got, tc.want)
		}
	}
}

// errorsAs keeps the assertion above readable without importing errors for one
// call.
func errorsAs(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError) //nolint:errorlint // exec returns it unwrapped
	if ok {
		*target = e
	}
	return ok
}
