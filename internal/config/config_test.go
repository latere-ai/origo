// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/limits"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

// testKey is a P-256 key in the form openssl ecparam -genkey writes: an
// EC PARAMETERS block, then the key.
func testKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}})
	return string(params) + string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func complete(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"ORIGO_S3_ENDPOINT":      "http://127.0.0.1:9000",
		"ORIGO_S3_REGION":        "us-east-1",
		"ORIGO_S3_BUCKET":        "origo",
		"ORIGO_S3_KEY":           "minioadmin",
		"ORIGO_S3_SECRET":        "minioadmin",
		"ORIGO_PUBLIC_URL":       "https://git.example.com/",
		"ORIGO_OIDC_ISSUERS":     "https://issuer.example",
		"ORIGO_AUTHORIZER_URL":   "https://authz.example",
		"ORIGO_AUTHORIZER_TOKEN": "s",
		"ORIGO_TOKEN_KEY":        testKey(t),
	}
}

var required = []string{
	"ORIGO_S3_ENDPOINT", "ORIGO_S3_REGION", "ORIGO_S3_BUCKET", "ORIGO_S3_KEY", "ORIGO_S3_SECRET",
	"ORIGO_PUBLIC_URL", "ORIGO_OIDC_ISSUERS", "ORIGO_AUTHORIZER_URL", "ORIGO_AUTHORIZER_TOKEN", "ORIGO_TOKEN_KEY",
}

func TestLoadNamesEveryMissingKeyInOneMessage(t *testing.T) {
	_, err := Load(env(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range required {
		if !strings.Contains(err.Error(), "missing "+key) {
			t.Errorf("message %q does not name %s", err, key)
		}
	}
	if strings.Count(err.Error(), "missing ") != len(required) {
		t.Errorf("message %q names the wrong number of keys", err)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	old := osHostname
	osHostname = func() (string, error) { return "node-1", nil }
	t.Cleanup(func() { osHostname = old })
	cfg, err := Load(env(complete(t)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != DefaultDataDir || cfg.PublicAddr != DefaultPublicAddr || cfg.InternalAddr != DefaultInternalAddr || cfg.GossipAddr != DefaultGossipAddr {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.SweepInterval != DefaultSweepInterval || cfg.SweepMinAge != DefaultSweepMinAge {
		t.Fatalf("sweep defaults not applied: %+v", cfg)
	}
	if cfg.RepairInterval != DefaultRepairInterval || cfg.RepairUnheard != DefaultRepairUnheard {
		t.Fatalf("repair defaults not applied: %+v", cfg)
	}
	if cfg.StorageTimeout != DefaultStorageTimeout || cfg.StaleMax != DefaultStaleMax {
		t.Fatalf("degraded-storage defaults not applied: %+v", cfg)
	}
	if cfg.NodeName != "node-1" {
		t.Fatalf("NodeName = %q", cfg.NodeName)
	}
	if cfg.PublicURL.String() != "https://git.example.com" {
		t.Fatalf("PublicURL = %q, trailing slash not trimmed", cfg.PublicURL)
	}
	if cfg.S3PathStyle || cfg.CacheBytes != 0 || cfg.OIDCInsecureIssuers != nil || cfg.Failpoint != "" {
		t.Fatalf("unexpected optional values: %+v", cfg)
	}
	// The endpoint LFS clients reach is the bucket's own by default
	// (spec 010).
	if cfg.S3PublicEndpoint != cfg.S3Endpoint {
		t.Fatalf("S3PublicEndpoint = %q, want %q", cfg.S3PublicEndpoint, cfg.S3Endpoint)
	}
	if len(cfg.OIDCIssuers) != 1 || cfg.AuthorizerURL != "https://authz.example" || cfg.TokenKey == nil || cfg.TokenKey.Curve != elliptic.P256() {
		t.Fatalf("spec 007 values: %+v", cfg)
	}
	if cfg.MaxGitProcs != limits.DefaultMaxGitProcs || cfg.RequestsPerMinute != limits.RequestsPerMinute {
		t.Fatalf("limits: %d procs and %d requests a minute, want the defaults %d and %d",
			cfg.MaxGitProcs, cfg.RequestsPerMinute, limits.DefaultMaxGitProcs, limits.RequestsPerMinute)
	}
}

func TestLoadReadsEveryOptionalValue(t *testing.T) {
	m := complete(t)
	m["ORIGO_S3_PATH_STYLE"] = "1"
	m["ORIGO_DATA_DIR"] = "/data/origo"
	m["ORIGO_CACHE_BYTES"] = "1024"
	m["ORIGO_OIDC_ISSUERS"] = "https://a.example/, https://b.example,"
	m["ORIGO_OIDC_INSECURE_ISSUERS"] = "http://stubs.example:8081/"
	m["ORIGO_EVENTS_URL"] = "https://events.example"
	m["ORIGO_EVENTS_SECRET"] = "e"
	m["ORIGO_NODE_NAME"] = "pod-7"
	m["ORIGO_GOSSIP_PEERS"] = "origod-headless"
	m["ORIGO_GOSSIP_SECRET"] = strings.Repeat("s", 32)
	m["ORIGO_PUBLIC_ADDR"] = "127.0.0.1:0"
	m["ORIGO_INTERNAL_ADDR"] = "127.0.0.1:1"
	m["ORIGO_GOSSIP_ADDR"] = "127.0.0.1:2"
	m["ORIGO_SWEEP_INTERVAL"] = "1s"
	m["ORIGO_SWEEP_MIN_AGE"] = "0s"
	m["ORIGO_STORAGE_TIMEOUT"] = "2s"
	m["ORIGO_STALE_MAX"] = "30s"
	m["ORIGO_FAILPOINT"] = "commit.before-index"
	m["ORIGO_REPAIR_INTERVAL"] = "10s"
	m["ORIGO_REPAIR_UNHEARD"] = "5s"
	m["ORIGO_S3_PUBLIC_ENDPOINT"] = "http://localhost:30900"
	m["ORIGO_MAX_GIT_PROCS"] = "8"
	m["ORIGO_REQUESTS_PER_MINUTE"] = "0"
	cfg, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.S3PathStyle || cfg.DataDir != "/data/origo" || cfg.CacheBytes != 1024 {
		t.Fatalf("storage values: %+v", cfg)
	}
	if cfg.S3PublicEndpoint != "http://localhost:30900" {
		t.Fatalf("S3PublicEndpoint = %q", cfg.S3PublicEndpoint)
	}
	if len(cfg.OIDCIssuers) != 2 || cfg.OIDCIssuers[0] != "https://a.example" || cfg.OIDCIssuers[1] != "https://b.example" {
		t.Fatalf("OIDCIssuers = %q", cfg.OIDCIssuers)
	}
	if len(cfg.OIDCInsecureIssuers) != 1 || cfg.OIDCInsecureIssuers[0] != "http://stubs.example:8081" {
		t.Fatalf("OIDCInsecureIssuers = %q", cfg.OIDCInsecureIssuers)
	}
	if cfg.AuthorizerURL != "https://authz.example" || cfg.AuthorizerToken != "s" || cfg.EventsURL != "https://events.example" || cfg.EventsSecret != "e" {
		t.Fatalf("spec 007/008 values: %+v", cfg)
	}
	if cfg.NodeName != "pod-7" || cfg.GossipPeers != "origod-headless" || cfg.GossipSecret != strings.Repeat("s", 32) {
		t.Fatalf("node values: %+v", cfg)
	}
	if cfg.PublicAddr != "127.0.0.1:0" || cfg.InternalAddr != "127.0.0.1:1" || cfg.GossipAddr != "127.0.0.1:2" {
		t.Fatalf("listen values: %+v", cfg)
	}
	if cfg.SweepInterval != time.Second || cfg.SweepMinAge != 0 || cfg.Failpoint != "commit.before-index" {
		t.Fatalf("development values: %+v", cfg)
	}
	if cfg.MaxGitProcs != 8 || cfg.RequestsPerMinute != 0 {
		t.Fatalf("limits: %d procs, %d requests a minute", cfg.MaxGitProcs, cfg.RequestsPerMinute)
	}
	if cfg.RepairInterval != 10*time.Second || cfg.RepairUnheard != 5*time.Second {
		t.Fatalf("repair values: %+v", cfg)
	}
	if cfg.StorageTimeout != 2*time.Second || cfg.StaleMax != 30*time.Second {
		t.Fatalf("degraded-storage values: %+v", cfg)
	}
}

// TestStorageTimeoutMustBeAboveZero: a zero deadline would fail every
// storage operation at once, so it is a problem in the one message
// beside a malformed value (spec 015).
func TestStorageTimeoutMustBeAboveZero(t *testing.T) {
	m := complete(t)
	m["ORIGO_STORAGE_TIMEOUT"] = "0"
	m["ORIGO_STALE_MAX"] = "soon"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"ORIGO_STORAGE_TIMEOUT must be above zero", "ORIGO_STALE_MAX must be a duration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
}

// TestEventsURLNeedsTheSecret is spec 008's criterion: the URL without
// the secret fails the start-up with the one message, in the one line
// beside every other problem; the secret without the URL, and neither,
// pass.
func TestEventsURLNeedsTheSecret(t *testing.T) {
	m := complete(t)
	m["ORIGO_EVENTS_URL"] = "https://events.example"
	_, err := Load(env(m))
	if err == nil || err.Error() != "configuration: ORIGO_EVENTS_SECRET is required with ORIGO_EVENTS_URL" {
		t.Fatalf("err = %v", err)
	}
	m["ORIGO_REPAIR_INTERVAL"] = "soon"
	_, err = Load(env(m))
	if err == nil || !strings.Contains(err.Error(), "ORIGO_EVENTS_SECRET is required with ORIGO_EVENTS_URL") || !strings.Contains(err.Error(), "ORIGO_REPAIR_INTERVAL must be a duration such as 10m") {
		t.Fatalf("not one message with both problems: %v", err)
	}
	delete(m, "ORIGO_REPAIR_INTERVAL")
	m["ORIGO_EVENTS_SECRET"] = "k"
	if cfg, err := Load(env(m)); err != nil || cfg.EventsURL != "https://events.example" || cfg.EventsSecret != "k" {
		t.Fatalf("both set: %+v, %v", cfg, err)
	}
	delete(m, "ORIGO_EVENTS_URL")
	if cfg, err := Load(env(m)); err != nil || cfg.EventsURL != "" {
		t.Fatalf("secret alone: %+v, %v", cfg, err)
	}
}

func TestLoadReportsMalformedValuesTogether(t *testing.T) {
	m := complete(t)
	m["ORIGO_PUBLIC_URL"] = "git.example.com"
	m["ORIGO_CACHE_BYTES"] = "lots"
	m["ORIGO_SWEEP_INTERVAL"] = "soon"
	m["ORIGO_SWEEP_MIN_AGE"] = "-1h"
	m["ORIGO_AUTHORIZER_URL"] = "authz.example"
	m["ORIGO_TOKEN_KEY"] = "not a key"
	m["ORIGO_OIDC_ISSUERS"] = "issuer.example"
	m["ORIGO_MAX_GIT_PROCS"] = "0"
	m["ORIGO_REQUESTS_PER_MINUTE"] = "-1"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"ORIGO_PUBLIC_URL must be an absolute URL", "ORIGO_CACHE_BYTES must be a positive integer",
		"ORIGO_SWEEP_INTERVAL must be a duration", "ORIGO_SWEEP_MIN_AGE must be a duration",
		"ORIGO_AUTHORIZER_URL must be an absolute", "ORIGO_TOKEN_KEY must be a PEM-encoded ECDSA P-256 private key",
		"ORIGO_OIDC_ISSUERS: issuer.example is not an absolute",
		"ORIGO_MAX_GIT_PROCS must be a positive integer",
		"ORIGO_REQUESTS_PER_MINUTE must be a non-negative integer",
	} {
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

// TestGossipSecretIsRequiredWithPeers is spec 002's rule for spec
// 005's two variables: peers without the secret is a missing key in
// the one message, a secret shorter than 32 bytes is malformed, the
// secret alone is read and unused, and neither is a single node.
func TestGossipSecretIsRequiredWithPeers(t *testing.T) {
	m := complete(t)
	m["ORIGO_GOSSIP_PEERS"] = "127.0.0.1:7946,127.0.0.1:7947"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "missing ORIGO_GOSSIP_SECRET") {
		t.Fatalf("peers without the secret: %v", err)
	}
	m["ORIGO_GOSSIP_SECRET"] = "short"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ORIGO_GOSSIP_SECRET must be at least 32 bytes") {
		t.Fatalf("short secret: %v", err)
	}
	m["ORIGO_GOSSIP_SECRET"] = strings.Repeat("k", 32)
	cfg, err := Load(env(m))
	if err != nil || cfg.GossipSecret != strings.Repeat("k", 32) || cfg.GossipPeers != "127.0.0.1:7946,127.0.0.1:7947" {
		t.Fatalf("peers with the secret: %+v %v", cfg, err)
	}
	delete(m, "ORIGO_GOSSIP_PEERS")
	if cfg, err := Load(env(m)); err != nil || cfg.GossipSecret == "" {
		t.Fatalf("the secret without peers: %+v %v", cfg, err)
	}
	delete(m, "ORIGO_GOSSIP_SECRET")
	if cfg, err := Load(env(m)); err != nil || cfg.GossipSecret != "" || cfg.GossipPeers != "" {
		t.Fatalf("a single node: %+v %v", cfg, err)
	}
}

func TestDevTokenIsRefused(t *testing.T) {
	m := complete(t)
	m["ORIGO_DEV_TOKEN"] = "dev"
	_, err := Load(env(m))
	if err == nil || !strings.Contains(err.Error(), "ORIGO_DEV_TOKEN is no longer read; remove it") {
		t.Fatalf("err = %v", err)
	}
	if err.Error() != "configuration: ORIGO_DEV_TOKEN is no longer read; remove it" {
		t.Fatalf("the dev token was not the one problem: %v", err)
	}
}

func TestInsecureIssuersNeedTheList(t *testing.T) {
	for _, iss := range []string{"http://127.0.0.1:8081", "http://localhost:8081", "http://[::1]:8081", "https://issuer.example"} {
		m := complete(t)
		m["ORIGO_OIDC_ISSUERS"] = iss
		if _, err := Load(env(m)); err != nil {
			t.Errorf("%s: %v", iss, err)
		}
	}
	m := complete(t)
	m["ORIGO_OIDC_ISSUERS"] = "http://origo-stubs.origo.svc:8081"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "http://origo-stubs.origo.svc:8081 uses http://") || !strings.Contains(err.Error(), "ORIGO_OIDC_INSECURE_ISSUERS") {
		t.Fatalf("a plain http issuer started: %v", err)
	}
	m["ORIGO_OIDC_INSECURE_ISSUERS"] = "http://origo-stubs.origo.svc:8081"
	cfg, err := Load(env(m))
	if err != nil || cfg.OIDCIssuers[0] != "http://origo-stubs.origo.svc:8081" {
		t.Fatalf("listed issuer refused: %v", err)
	}
	m["ORIGO_OIDC_ISSUERS"] = "ftp://issuer.example"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ftp://issuer.example is not an absolute http or https URL") {
		t.Fatalf("another scheme started: %v", err)
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
