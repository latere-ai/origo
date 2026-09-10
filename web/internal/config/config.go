// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package config reads the interface's settings from the environment.
//
// Every value is a setting an operator gives the process. Nothing here is
// derived from a request: the clone URLs, the redirect URI, and the address
// of the Origo installation are configuration, so a forged Host header
// cannot move them (spec 023, "TestCloneURLsComeFromConfiguration").
package config

import (
	"cmp"
	"fmt"
	"net/url"
	"os"
	"strings"

	"latere.ai/x/pkg/authkit/oidc"
)

// EnvPrefix is the prefix of every variable this service reads, including
// the ones authkit/oidc reads on its behalf.
const EnvPrefix = "ORIGOWEB"

// Config is the whole configuration of one process.
type Config struct {
	// Addr is the listener address.
	Addr string

	// OrigoURL is the base URL of the Origo installation, the one address
	// this service talks to.
	OrigoURL *url.URL

	// PublicURL is this service's own base URL, used for absolute links.
	PublicURL *url.URL

	// CloneHost is what the HTTPS clone URL on the overview names. It
	// defaults to OrigoURL, for the common installation where the git
	// surface and the API surface are one address.
	CloneHost *url.URL

	// SSHCloneHost is the host of the SSH clone form, empty when the
	// installation has no SSH surface; the overview then shows the HTTPS
	// form alone.
	SSHCloneHost string

	// KeysURL is the key management surface. No component owns one today
	// (spec 023, spec 024), so it is unset by default and the key screen
	// and its navigation entry are absent while it is.
	KeysURL string

	// IssuerName is what the sign-in button names, for an installation
	// whose people know their identity provider by name.
	IssuerName string

	// OIDC is the relying-party configuration authkit reads.
	OIDC oidc.Config
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	c := Config{
		Addr:         cmp.Or(os.Getenv(EnvPrefix+"_ADDR"), ":8080"),
		SSHCloneHost: strings.TrimSpace(os.Getenv(EnvPrefix + "_SSH_CLONE_HOST")),
		KeysURL:      strings.TrimSpace(os.Getenv(EnvPrefix + "_KEYS_URL")),
		IssuerName:   strings.TrimSpace(os.Getenv(EnvPrefix + "_ISSUER_NAME")),
		OIDC:         oidc.LoadConfigWithPrefix(EnvPrefix),
	}

	var err error
	if c.OrigoURL, err = requiredURL(EnvPrefix + "_ORIGO_URL"); err != nil {
		return Config{}, err
	}
	if c.PublicURL, err = requiredURL(EnvPrefix + "_PUBLIC_URL"); err != nil {
		return Config{}, err
	}
	if raw := strings.TrimSpace(os.Getenv(EnvPrefix + "_CLONE_HOST")); raw != "" {
		if c.CloneHost, err = parseURL(EnvPrefix+"_CLONE_HOST", raw); err != nil {
			return Config{}, err
		}
	} else {
		c.CloneHost = c.OrigoURL
	}

	if c.OIDC.RedirectURL == "" {
		c.OIDC.RedirectURL = strings.TrimRight(c.PublicURL.String(), "/") + "/auth/callback"
	}
	return c, nil
}

func requiredURL(name string) (*url.URL, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	return parseURL(name, raw)
}

func parseURL(name, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s: want an http or https URL, got %q", name, raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%s: want a URL with a host, got %q", name, raw)
	}
	return u, nil
}

// CloneHTTPS is the HTTPS clone URL of one repository, built from
// configuration alone.
func (c Config) CloneHTTPS(owner, slug string) string {
	return fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(c.CloneHost.String(), "/"), owner, slug)
}

// CloneSSH is the SSH clone URL of one repository, empty when the
// installation has no SSH surface.
func (c Config) CloneSSH(owner, slug string) string {
	if c.SSHCloneHost == "" {
		return ""
	}
	return fmt.Sprintf("git@%s:%s/%s.git", c.SSHCloneHost, owner, slug)
}

// RepoAPIURL is the address of one repository on the Origo installation:
// the read API's own entry point, which an agent with a token calls
// directly rather than through this interface.
func (c Config) RepoAPIURL(id string) string {
	return fmt.Sprintf("%s/v1/repos/%s", strings.TrimRight(c.OrigoURL.String(), "/"), id)
}

// ArchiveURL is the address of a repository's archive on the Origo
// installation. The link points at Origo directly, so a large tarball never
// passes through this service.
func (c Config) ArchiveURL(id, sha string) string {
	return fmt.Sprintf("%s/v1/repos/%s/archive/%s.tar.gz", strings.TrimRight(c.OrigoURL.String(), "/"), id, sha)
}
