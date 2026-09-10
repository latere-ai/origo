// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunRefusesToStartWithoutItsSettings asserts that a missing setting
// stops the process at start-up rather than at the first request.
func TestRunRefusesToStartWithoutItsSettings(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no installation address": {"ORIGOWEB_PUBLIC_URL": "https://code.example"},
		"no identity provider": {
			"ORIGOWEB_ORIGO_URL": "https://git.example", "ORIGOWEB_PUBLIC_URL": "https://code.example",
		},
	} {
		clearEnv(t)
		for k, v := range env {
			t.Setenv(k, v)
		}
		if err := run(); err == nil {
			t.Errorf("%s: the process started anyway", name)
		}
	}
}

// TestRunServesAndStops asserts the whole life of the process: it listens,
// answers its liveness probe, and stops on the signal an orchestrator sends.
func TestRunServesAndStops(t *testing.T) {
	clearEnv(t)
	port := freePort(t)
	t.Setenv("ORIGOWEB_ADDR", port)
	t.Setenv("ORIGOWEB_ORIGO_URL", "https://git.example")
	t.Setenv("ORIGOWEB_PUBLIC_URL", "https://code.example")
	t.Setenv("ORIGOWEB_AUTH_CLIENT_ID", "origoweb")
	t.Setenv("ORIGOWEB_AUTH_COOKIE_KEY", "0123456789abcdef0123456789abcdef")

	done := make(chan error, 1)
	go func() { done <- run() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1"+port, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener never came up: %v", err)
		}
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the process stopped with %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("the process did not stop on the signal")
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr := l.Addr().String()
	return addr[strings.LastIndex(addr, ":"):]
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ORIGOWEB_ADDR", "ORIGOWEB_ORIGO_URL", "ORIGOWEB_PUBLIC_URL", "ORIGOWEB_CLONE_HOST",
		"ORIGOWEB_SSH_CLONE_HOST", "ORIGOWEB_KEYS_URL", "ORIGOWEB_ISSUER_NAME",
		"ORIGOWEB_AUTH_CLIENT_ID", "ORIGOWEB_AUTH_COOKIE_KEY", "ORIGOWEB_AUTH_REDIRECT_URL",
		"AUTH_CLIENT_ID", "AUTH_COOKIE_KEY", "AUTH_URL", "AUTH_REDIRECT_URL",
	} {
		t.Setenv(k, "")
	}
}
