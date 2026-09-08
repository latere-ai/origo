// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEgressAllowPinsOnlyExactHosts is spec 016's criterion for the
// list: an exact host may carry one IP literal as host=address, which
// is recorded as its pinned address; a pinned address on a wildcard,
// or one that is not an IP literal, fails the start-up in the one
// message; the hosts are normalized; unset is an empty list.
func TestEgressAllowPinsOnlyExactHosts(t *testing.T) {
	m := complete(t)
	m["ORIGO_EGRESS_ALLOW"] = " Origo-Stubs.origo.svc.=10.96.0.42, *.Example.COM ,github.com, v6.example=::ffff:10.0.0.7"
	m["ORIGO_CLUSTER_CIDRS"] = "10.96.0.1/16, 10.244.0.0/16"
	cfg, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"origo-stubs.origo.svc", "*.example.com", "github.com", "v6.example"}; !slices.Equal(cfg.EgressAllow, want) {
		t.Fatalf("EgressAllow = %q, want %q", cfg.EgressAllow, want)
	}
	if got := cfg.EgressPinned; len(got) != 2 || got["origo-stubs.origo.svc"] != netip.MustParseAddr("10.96.0.42") || got["v6.example"] != netip.MustParseAddr("10.0.0.7") {
		t.Fatalf("EgressPinned = %v", got)
	}
	if want := []netip.Prefix{netip.MustParsePrefix("10.96.0.0/16"), netip.MustParsePrefix("10.244.0.0/16")}; !slices.Equal(cfg.ClusterCIDRs, want) {
		t.Fatalf("ClusterCIDRs = %v, want %v", cfg.ClusterCIDRs, want)
	}
	if cfg.EgressCA != nil {
		t.Fatal("EgressCA set without the bundle")
	}

	// Unset: nothing is admitted and nothing is pinned.
	cfg, err = Load(env(complete(t)))
	if err != nil || len(cfg.EgressAllow) != 0 || len(cfg.EgressPinned) != 0 || len(cfg.ClusterCIDRs) != 0 {
		t.Fatalf("unset: %v %v %v", cfg.EgressAllow, cfg.EgressPinned, err)
	}

	// Every malformed entry is one problem of the one message.
	m = complete(t)
	m["ORIGO_EGRESS_ALLOW"] = "*.example.com=10.96.0.42,github.com=not-an-address,bad host,ok.example"
	m["ORIGO_CLUSTER_CIDRS"] = "10.96.0.0/16,not-a-range"
	_, err = Load(env(m))
	if err == nil {
		t.Fatal("malformed entries accepted")
	}
	for _, want := range []string{
		"ORIGO_EGRESS_ALLOW: *.example.com=10.96.0.42 pins an address on a wildcard",
		`ORIGO_EGRESS_ALLOW: github.com=not-an-address pins "not-an-address", which is not an IP literal`,
		"ORIGO_EGRESS_ALLOW: bad host is not a hostname or a *. wildcard",
		"ORIGO_CLUSTER_CIDRS: not-a-range is not a CIDR range",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
	if strings.Count(err.Error(), ";") != 3 {
		t.Fatalf("expected four problems: %s", err)
	}
}

// selfSignedCA writes a PEM certificate a test uses as a bundle.
func selfSignedCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEgressCABundleIsReadAtStartup: ORIGO_EGRESS_CA_BUNDLE names a PEM
// file whose certificates join the system roots; a missing file or one
// without a certificate is a problem of the one message.
func TestEgressCABundleIsReadAtStartup(t *testing.T) {
	m := complete(t)
	m["ORIGO_EGRESS_CA_BUNDLE"] = selfSignedCA(t)
	cfg, err := Load(env(m))
	if err != nil || cfg.EgressCA == nil {
		t.Fatalf("bundle: %v", err)
	}
	m["ORIGO_EGRESS_CA_BUNDLE"] = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ORIGO_EGRESS_CA_BUNDLE: ") {
		t.Fatalf("missing bundle: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	m["ORIGO_EGRESS_CA_BUNDLE"] = empty
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "holds no certificate") {
		t.Fatalf("empty bundle: %v", err)
	}
}
