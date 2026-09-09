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
	"strings"
	"syscall"

	"latere.ai/x/pkg/otel"

	"github.com/latere-ai/origo/internal/config"
	versionpkg "github.com/latere-ai/origo/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run dispatches the subcommand and returns the process exit code, so
// tests drive it without a subprocess: 0 on a clean stop, 1 on a
// start-up or runtime failure, 2 on a usage error.
//
// The subcommand table is spec 002's. serve is the default and reads
// the node's whole configuration; check reads the same table and reaches
// everything in it, so it runs wherever the node runs and nowhere else;
// migrate is spec 014's batch client and reads three variables and none
// of the node's, so the load of the node's table happens inside each
// subcommand and an operator running migrate from a laptop is not asked
// for a bucket.
func run(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	name, rest := subcommand(args)
	switch name {
	case "", "serve":
		return serve(ctx, rest, getenv, stdout, stderr)
	case "check":
		return check(ctx, rest, getenv, stdout, stderr)
	case "migrate":
		return migrate(ctx, rest, getenv, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "origod: unknown subcommand %q; serve (the default), check, and migrate\n", name)
		return 2
	}
}

// subcommand is spec 002's rule: the first argument that does not start
// with a dash names the subcommand, and the arguments around it are the
// subcommand's own.
func subcommand(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			return a, append(rest, args[i+1:]...)
		}
	}
	return "", args
}

// versionFlag adds the -version flag every subcommand shares.
func versionFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("version", false, "print the build identity and exit")
}

// serve is the node: the listeners and the loops of spec 002.
func serve(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("origod serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := versionFlag(fs)
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
