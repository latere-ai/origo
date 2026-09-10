// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command origo reads and changes a repository on an Origo installation
// without cloning it (spec 025).
//
// This file is the entry point and holds wiring only: the environment, the
// signal context, and the exit code. The commands live in internal/origocli
// and the contract they call in internal/origoclient, so everything a test
// wants to reach is reachable without a subprocess.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/latere-ai/origo/internal/origocli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(origocli.Run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
