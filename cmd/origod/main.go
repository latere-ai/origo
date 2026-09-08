// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command origod is the Origo server: git hosting with a write-ahead log in
// object storage as the source of truth. This file is the entry point and
// holds wiring only: configuration, the listeners, and the run group. The
// behaviour lives in the packages under internal/.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"latere.ai/x/pkg/otel"

	"github.com/latere-ai/origo/internal/config"
	versionpkg "github.com/latere-ai/origo/internal/version"
)

// version is set by the release pipeline with -X main.version=<tag>. It
// wins over the development marker so the served /version is the tag.
var version string

func main() {
	if version != "" {
		versionpkg.Version = version
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run parses the command line, loads the configuration, and serves until
// ctx ends. It returns the process exit code so tests drive it without a
// subprocess: 0 on a clean stop, 1 on a start-up or runtime failure, 2 on a
// usage error.
func run(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("origod", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, versionpkg.String())
		return 0
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "origod:", err)
		return 1
	}
	if err := cfg.Resolve(); err != nil {
		_, _ = fmt.Fprintln(stderr, "origod:", err)
		return 1
	}
	logger, flush := bootstrap(ctx, stdout)
	n, err := newNode(cfg, logger)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "origod:", err)
		return 1
	}
	n.flushTelemetry = flush
	if err := n.run(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "origod:", err)
		return 1
	}
	return 0
}

// bootstrap wires telemetry (spec 011): the logger every line goes
// through, teed to the OTLP bridge when OTEL_EXPORTER_OTLP_ENDPOINT is
// set, the tracer provider the spans go to, and the flush the node runs
// at the end of its drain. The local handler writes to stdout, because
// pkg/otel defaults to standard error and the node's lines stay on
// standard output (spec 002).
//
// Without the endpoint nothing is exported and the logger is the same
// JSON handler the node had before, so this is safe in every mode.
func bootstrap(ctx context.Context, stdout io.Writer) (*slog.Logger, func(context.Context) error) {
	logger, flush, err := otel.Bootstrap(ctx, otel.Config{
		ServiceName: "origod",
		Version:     versionpkg.Version,
		Replica:     otel.Replica(),
		Stdout:      slog.NewJSONHandler(stdout, nil),
	})
	if err != nil {
		// The local handler is always usable; only the OTLP log bridge
		// failed, and the node serves without it.
		logger.WarnContext(ctx, "telemetry: the log bridge did not start", "error", err)
	}
	return logger, flush
}
