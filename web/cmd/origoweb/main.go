// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Command origoweb serves a read-only browsing interface over one Origo
// installation.
//
// It is a pure client of Origo's read API. It holds no repository, runs no
// git, reads no object storage, and decides nothing about who may see what:
// it sends the signed-in person's own token to the installation and renders
// what comes back.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"latere.ai/x/pkg/otel"

	"github.com/latere-ai/origo-web/internal/config"
	"github.com/latere-ai/origo-web/internal/origo"
	"github.com/latere-ai/origo-web/internal/session"
	"github.com/latere-ai/origo-web/internal/web"
)

// Build metadata, set by -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("origoweb: stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	sessions, err := session.New(cfg.OIDC)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: web.New(web.Options{
			Config:    cfg,
			Sessions:  sessions,
			API:       origo.New(cfg.OrigoURL, otel.HTTPClient()),
			Version:   Version,
			Commit:    Commit,
			BuildTime: Date,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		slog.Info("origoweb: listening", "addr", cfg.Addr, "origo", cfg.OrigoURL.String(), "version", Version)
		errs <- srv.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
