// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/config"
)

// moduleRoot is the checkout, resolved from this file with
// runtime.Caller and never from the working directory, so the gate that
// runs the suite from an empty directory finds the files.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// TestEgressAdmitsNoLoopbackInProduction is spec 016's criterion for a
// production configuration: the node built from a configuration whose
// ORIGO_EGRESS_ALLOW names a host that resolves to 127.0.0.1 (a name
// under .localhost, which the resolver answers with loopback by RFC
// 6761 and no DNS query) refuses it
// with details.reason "egress" and opens no connection, and no variable
// of spec 002's table sets the AllowLoopback seam.
func TestEgressAdmitsNoLoopbackInProduction(t *testing.T) {
	env := testEnv(t)
	env["ORIGO_EGRESS_ALLOW"] = "source.localhost"
	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	n, err := newNode(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()
	_, err = n.egress.DialContext(context.Background(), "tcp", "source.localhost:"+strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:"))
	ee, ok := errors.AsType[*api.EgressError](err)
	if !ok || ee.Details()["reason"] != "egress" || !strings.Contains(ee.Cause, "loopback") {
		t.Fatalf("a loopback source was not refused: %v", err)
	}
	select {
	case <-accepted:
		t.Fatal("the refusal opened a connection")
	default:
	}
	// The configuration table of spec 002 has no variable for the seam.
	spec, err := os.ReadFile(filepath.Join(moduleRoot(t), "specs", "002-repository-scaffold.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range regexp.MustCompile(`(?m)^\| `+"`ORIGO_"+`[^\n]*$`).FindAllString(string(spec), -1) {
		if strings.Contains(line, "AllowLoopback") {
			t.Fatalf("a variable of the table sets the seam: %s", line)
		}
	}
}

// TestSecurityPolicyIsPresent: SECURITY.md exists at the module root and
// names the report address.
func TestSecurityPolicyIsPresent(t *testing.T) {
	policy, err := os.ReadFile(filepath.Join(moduleRoot(t), "SECURITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"security@latere.ai", "three business days", "thirty days"} {
		if !strings.Contains(string(policy), want) {
			t.Errorf("SECURITY.md lacks %q", want)
		}
	}
}
