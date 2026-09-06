// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func complete() map[string]string {
	return map[string]string{
		"ORIGO_S3_ENDPOINT": "http://127.0.0.1:9000",
		"ORIGO_S3_REGION":   "us-east-1",
		"ORIGO_S3_BUCKET":   "origo",
		"ORIGO_S3_KEY":      "minioadmin",
		"ORIGO_S3_SECRET":   "minioadmin",
		"ORIGO_PUBLIC_URL":  "https://git.example.com/",
		"ORIGO_DEV_TOKEN":   "dev-token",
	}
}

func TestLoadNamesEveryMissingKeyInOneMessage(t *testing.T) {
	_, err := Load(env(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"ORIGO_S3_ENDPOINT", "ORIGO_S3_REGION", "ORIGO_S3_BUCKET", "ORIGO_S3_KEY", "ORIGO_S3_SECRET", "ORIGO_PUBLIC_URL", "ORIGO_DEV_TOKEN"} {
		if !strings.Contains(err.Error(), "missing "+key) {
			t.Errorf("message %q does not name %s", err, key)
		}
	}
	if strings.Count(err.Error(), "missing ") != 7 {
		t.Errorf("message %q names the wrong number of keys", err)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	old := osHostname
	osHostname = func() (string, error) { return "node-1", nil }
	t.Cleanup(func() { osHostname = old })
	cfg, err := Load(env(complete()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != DefaultDataDir || cfg.PublicAddr != DefaultPublicAddr || cfg.InternalAddr != DefaultInternalAddr || cfg.GossipAddr != DefaultGossipAddr {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.SweepInterval != DefaultSweepInterval || cfg.SweepMinAge != DefaultSweepMinAge {
		t.Fatalf("sweep defaults not applied: %+v", cfg)
	}
	if cfg.NodeName != "node-1" {
		t.Fatalf("NodeName = %q", cfg.NodeName)
	}
	if cfg.PublicURL.String() != "https://git.example.com" {
		t.Fatalf("PublicURL = %q, trailing slash not trimmed", cfg.PublicURL)
	}
	if cfg.S3PathStyle || cfg.CacheBytes != 0 || cfg.OIDCIssuers != nil || cfg.Failpoint != "" {
		t.Fatalf("unexpected optional values: %+v", cfg)
	}
}

func TestLoadReadsEveryOptionalValue(t *testing.T) {
	m := complete()
	m["ORIGO_S3_PATH_STYLE"] = "1"
	m["ORIGO_DATA_DIR"] = "/data/origo"
	m["ORIGO_CACHE_BYTES"] = "1024"
	m["ORIGO_OIDC_ISSUERS"] = "https://a.example, https://b.example,"
	m["ORIGO_AUTHORIZER_URL"] = "https://authz.example"
	m["ORIGO_AUTHORIZER_TOKEN"] = "s"
	m["ORIGO_EVENTS_URL"] = "https://events.example"
	m["ORIGO_EVENTS_SECRET"] = "e"
	m["ORIGO_NODE_NAME"] = "pod-7"
	m["ORIGO_GOSSIP_PEERS"] = "origod-headless"
	m["ORIGO_PUBLIC_ADDR"] = "127.0.0.1:0"
	m["ORIGO_INTERNAL_ADDR"] = "127.0.0.1:1"
	m["ORIGO_GOSSIP_ADDR"] = "127.0.0.1:2"
	m["ORIGO_SWEEP_INTERVAL"] = "1s"
	m["ORIGO_SWEEP_MIN_AGE"] = "0s"
	m["ORIGO_FAILPOINT"] = "commit.before-index"
	cfg, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.S3PathStyle || cfg.DataDir != "/data/origo" || cfg.CacheBytes != 1024 {
		t.Fatalf("storage values: %+v", cfg)
	}
	if len(cfg.OIDCIssuers) != 2 || cfg.OIDCIssuers[1] != "https://b.example" {
		t.Fatalf("OIDCIssuers = %q", cfg.OIDCIssuers)
	}
	if cfg.AuthorizerURL != "https://authz.example" || cfg.AuthorizerToken != "s" || cfg.EventsURL != "https://events.example" || cfg.EventsSecret != "e" {
		t.Fatalf("spec 007/008 values: %+v", cfg)
	}
	if cfg.NodeName != "pod-7" || cfg.GossipPeers != "origod-headless" {
		t.Fatalf("node values: %+v", cfg)
	}
	if cfg.PublicAddr != "127.0.0.1:0" || cfg.InternalAddr != "127.0.0.1:1" || cfg.GossipAddr != "127.0.0.1:2" {
		t.Fatalf("listen values: %+v", cfg)
	}
	if cfg.SweepInterval != time.Second || cfg.SweepMinAge != 0 || cfg.Failpoint != "commit.before-index" {
		t.Fatalf("development values: %+v", cfg)
	}
}

func TestLoadReportsMalformedValuesTogether(t *testing.T) {
	m := complete()
	m["ORIGO_PUBLIC_URL"] = "git.example.com"
	m["ORIGO_CACHE_BYTES"] = "lots"
	m["ORIGO_SWEEP_INTERVAL"] = "soon"
	m["ORIGO_SWEEP_MIN_AGE"] = "-1h"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"ORIGO_PUBLIC_URL must be an absolute URL", "ORIGO_CACHE_BYTES must be a positive integer", "ORIGO_SWEEP_INTERVAL must be a duration", "ORIGO_SWEEP_MIN_AGE must be a duration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	m["ORIGO_PUBLIC_URL"] = "http://%zz"
	m["ORIGO_CACHE_BYTES"] = "0"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ORIGO_PUBLIC_URL must be") || !strings.Contains(err.Error(), "ORIGO_CACHE_BYTES must be") {
		t.Fatalf("unparsable URL and zero cache not reported: %v", err)
	}
}

func TestHostnameFallsBackToTheBinaryName(t *testing.T) {
	old := osHostname
	t.Cleanup(func() { osHostname = old })
	osHostname = func() (string, error) { return "", errors.New("no hostname") }
	if h := hostname(); h != "origod" {
		t.Fatalf("hostname = %q", h)
	}
	osHostname = old
	if h := hostname(); h == "" {
		t.Fatal("hostname is empty")
	}
}

func TestResolveCreatesTheDataDirAndDerivesTheCache(t *testing.T) {
	cfg := &Config{DataDir: filepath.Join(t.TempDir(), "origo")}
	if err := cfg.Resolve(); err != nil {
		t.Fatal(err)
	}
	if cfg.CacheBytes <= 0 {
		t.Fatalf("CacheBytes = %d", cfg.CacheBytes)
	}
	explicit := &Config{DataDir: cfg.DataDir, CacheBytes: 7}
	if err := explicit.Resolve(); err != nil || explicit.CacheBytes != 7 {
		t.Fatalf("explicit cache changed: %d, %v", explicit.CacheBytes, err)
	}
}

func TestResolveReportsAnUnusableDataDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := writeFile(file); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: filepath.Join(file, "sub")}
	if err := cfg.Resolve(); err == nil || !strings.Contains(err.Error(), "ORIGO_DATA_DIR") {
		t.Fatalf("err = %v", err)
	}
	old := diskSize
	diskSize = func(string) (int64, error) { return 0, errors.New("statfs failed") }
	t.Cleanup(func() { diskSize = old })
	cfg = &Config{DataDir: t.TempDir()}
	if err := cfg.Resolve(); err == nil || !strings.Contains(err.Error(), "statfs failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiskSizeReportsAMissingPath(t *testing.T) {
	if _, err := diskSize(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error")
	}
}
