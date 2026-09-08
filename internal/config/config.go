// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package config reads the typed configuration of origod from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/limits"
)

// Defaults for the optional variables.
const (
	DefaultDataDir       = "/var/lib/origo"
	DefaultPublicAddr    = ":8080"
	DefaultInternalAddr  = ":8081"
	DefaultGossipAddr    = ":7946"
	DefaultSweepInterval = 10 * time.Minute
	DefaultSweepMinAge   = time.Hour
	// The event repair sweep of spec 008: how often it runs and how long
	// a node must be unheard before its journals are repaired.
	DefaultRepairInterval = 10 * time.Minute
	DefaultRepairUnheard  = 5 * time.Minute
	// The degraded-storage values of spec 015: the deadline of one
	// object storage operation and how long a warm repository is served
	// from the local copy while the read breaker is open.
	DefaultStorageTimeout = 10 * time.Second
	DefaultStaleMax       = 5 * time.Minute
	// MinGossipSecretBytes is the shortest ORIGO_GOSSIP_SECRET accepted
	// (spec 002): the key of an HMAC-SHA256 is at least its output size.
	MinGossipSecretBytes = 32
	// Prefix under which every object of every repository lives. Spec 001
	// fixes it; it is not configurable.
	Prefix = "origo/"
)

// Config is the resolved configuration. Field names follow the variable
// names in spec 002 without the ORIGO_ prefix.
type Config struct {
	// The bucket. Required.
	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3Key       string
	S3Secret    string
	S3PathStyle bool
	// S3PublicEndpoint is the bucket endpoint LFS clients can reach; the
	// presigned URLs of spec 010 are signed against it. S3Endpoint when
	// unset, which is the deployment where the node and the client see
	// the bucket at the same name.
	S3PublicEndpoint string

	// DataDir holds the repository cache. It must be a local disk.
	DataDir string
	// CacheBytes is the eviction ceiling of the repository cache. Zero
	// means 80% of the disk that holds DataDir, resolved at start-up.
	CacheBytes int64

	// PublicURL is the origin clients see, used in clone URLs, event
	// payloads, and as the issuer of repository-bound tokens. Required.
	PublicURL *url.URL

	// Spec 007 identity and authorization. Required.
	OIDCIssuers         []string
	OIDCInsecureIssuers []string
	AuthorizerURL       string
	AuthorizerToken     string
	// TokenKey is the ECDSA P-256 key of ORIGO_TOKEN_KEY that signs
	// repository-bound tokens; required in every mode.
	TokenKey *ecdsa.PrivateKey

	// Spec 008 push events. Optional; the secret is required with the
	// URL, because an unsigned delivery is one the sink cannot trust.
	EventsURL    string
	EventsSecret string
	// RepairInterval is how often the event repair sweep runs and
	// RepairUnheard how long a node must be unheard before another node
	// repairs the events its journals name (spec 008).
	RepairInterval time.Duration
	RepairUnheard  time.Duration

	// NodeName identifies the node in gossip and placement (spec 005). The
	// host name by default, which is the pod name in Kubernetes.
	NodeName string
	// GossipPeers names the other nodes (spec 005): a comma separated
	// list of host:port entries, or one DNS name that resolves to every
	// node on the port of GossipAddr. Optional; empty is a single node.
	GossipPeers string
	// GossipSecret is the key of the HMAC every gossip datagram carries
	// (spec 005), at least MinGossipSecretBytes long. Required when
	// GossipPeers is set; read and unused without peers.
	GossipSecret string

	// Listen addresses. Spec 002 fixes the ports; the addresses are
	// variables so a test binds an ephemeral port and two checkouts run
	// side by side.
	PublicAddr   string
	InternalAddr string
	GossipAddr   string

	// Sweeper schedule (spec 004). The interval between sweeps and the age
	// an orphan must reach before it is deleted. Development knobs: the
	// defaults are the spec's values.
	SweepInterval time.Duration
	SweepMinAge   time.Duration

	// StorageTimeout is ORIGO_STORAGE_TIMEOUT (spec 015), the deadline of
	// one object storage operation; StaleMax is ORIGO_STALE_MAX, how long
	// a warm repository is served from the local copy while the read
	// breaker is open, measured from its last currency check that
	// answered.
	StorageTimeout time.Duration
	StaleMax       time.Duration

	// MaxGitProcs is ORIGO_MAX_GIT_PROCS: the git subprocesses this node
	// runs at once (spec 012).
	MaxGitProcs int

	// Failpoint names an injected failure for the end-to-end suite, for
	// example "commit.before-index". Empty in every deployment.
	Failpoint string
}

// Getenv is the source of variables; a test substitutes a map.
type Getenv func(string) string

// Load reads the configuration from getenv. It collects every problem and
// returns them as one error so the start-up message names all of them.
func Load(getenv Getenv) (*Config, error) {
	var problems []string
	missing := func(key string) string {
		v := getenv(key)
		if v == "" {
			problems = append(problems, "missing "+key)
		}
		return v
	}
	cfg := &Config{
		S3Endpoint:      missing("ORIGO_S3_ENDPOINT"),
		S3Region:        missing("ORIGO_S3_REGION"),
		S3Bucket:        missing("ORIGO_S3_BUCKET"),
		S3Key:           missing("ORIGO_S3_KEY"),
		S3Secret:        missing("ORIGO_S3_SECRET"),
		S3PathStyle:     getenv("ORIGO_S3_PATH_STYLE") == "1",
		DataDir:         orDefault(getenv("ORIGO_DATA_DIR"), DefaultDataDir),
		AuthorizerToken: missing("ORIGO_AUTHORIZER_TOKEN"),
		EventsURL:       getenv("ORIGO_EVENTS_URL"),
		EventsSecret:    getenv("ORIGO_EVENTS_SECRET"),
		NodeName:        getenv("ORIGO_NODE_NAME"),
		GossipPeers:     getenv("ORIGO_GOSSIP_PEERS"),
		GossipSecret:    getenv("ORIGO_GOSSIP_SECRET"),
		PublicAddr:      orDefault(getenv("ORIGO_PUBLIC_ADDR"), DefaultPublicAddr),
		InternalAddr:    orDefault(getenv("ORIGO_INTERNAL_ADDR"), DefaultInternalAddr),
		GossipAddr:      orDefault(getenv("ORIGO_GOSSIP_ADDR"), DefaultGossipAddr),
		Failpoint:       getenv("ORIGO_FAILPOINT"),
	}
	cfg.S3PublicEndpoint = orDefault(getenv("ORIGO_S3_PUBLIC_ENDPOINT"), cfg.S3Endpoint)
	if getenv("ORIGO_DEV_TOKEN") != "" {
		problems = append(problems, "ORIGO_DEV_TOKEN is no longer read; remove it")
	}
	if raw := missing("ORIGO_PUBLIC_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			problems = append(problems, "ORIGO_PUBLIC_URL must be an absolute URL such as https://git.example.com")
		} else {
			u.Path = strings.TrimRight(u.Path, "/")
			cfg.PublicURL = u
		}
	}
	cfg.OIDCInsecureIssuers = list(getenv("ORIGO_OIDC_INSECURE_ISSUERS"))
	cfg.OIDCIssuers = list(missing("ORIGO_OIDC_ISSUERS"))
	for _, iss := range cfg.OIDCIssuers {
		if problem := checkIssuer(iss, cfg.OIDCInsecureIssuers); problem != "" {
			problems = append(problems, problem)
		}
	}
	if raw := missing("ORIGO_AUTHORIZER_URL"); raw != "" {
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			problems = append(problems, "ORIGO_AUTHORIZER_URL must be an absolute http or https URL")
		} else {
			cfg.AuthorizerURL = raw
		}
	}
	if raw := missing("ORIGO_TOKEN_KEY"); raw != "" {
		key, err := auth.ParseKey(raw)
		if err != nil {
			problems = append(problems, "ORIGO_TOKEN_KEY must be a PEM-encoded ECDSA P-256 private key: "+err.Error())
		}
		cfg.TokenKey = key
	}
	cfg.MaxGitProcs = limits.DefaultMaxGitProcs
	if raw := getenv("ORIGO_MAX_GIT_PROCS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			problems = append(problems, "ORIGO_MAX_GIT_PROCS must be a positive integer")
		} else {
			cfg.MaxGitProcs = n
		}
	}
	if raw := getenv("ORIGO_CACHE_BYTES"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			problems = append(problems, "ORIGO_CACHE_BYTES must be a positive integer number of bytes")
		}
		cfg.CacheBytes = n
	}
	// The secret is required whenever there are peers, because a
	// datagram without a MAC is dropped and the node would never hear
	// them; a single node runs with neither (spec 005).
	switch {
	case cfg.GossipPeers != "" && cfg.GossipSecret == "":
		problems = append(problems, "missing ORIGO_GOSSIP_SECRET")
	case cfg.GossipSecret != "" && len(cfg.GossipSecret) < MinGossipSecretBytes:
		problems = append(problems, fmt.Sprintf("ORIGO_GOSSIP_SECRET must be at least %d bytes", MinGossipSecretBytes))
	}
	cfg.SweepInterval = duration(getenv, "ORIGO_SWEEP_INTERVAL", DefaultSweepInterval, &problems)
	cfg.SweepMinAge = duration(getenv, "ORIGO_SWEEP_MIN_AGE", DefaultSweepMinAge, &problems)
	cfg.RepairInterval = duration(getenv, "ORIGO_REPAIR_INTERVAL", DefaultRepairInterval, &problems)
	cfg.RepairUnheard = duration(getenv, "ORIGO_REPAIR_UNHEARD", DefaultRepairUnheard, &problems)
	cfg.StorageTimeout = duration(getenv, "ORIGO_STORAGE_TIMEOUT", DefaultStorageTimeout, &problems)
	cfg.StaleMax = duration(getenv, "ORIGO_STALE_MAX", DefaultStaleMax, &problems)
	if cfg.StorageTimeout == 0 {
		problems = append(problems, "ORIGO_STORAGE_TIMEOUT must be above zero")
	}
	if cfg.EventsURL != "" && cfg.EventsSecret == "" {
		problems = append(problems, "ORIGO_EVENTS_SECRET is required with ORIGO_EVENTS_URL")
	}
	if cfg.NodeName == "" {
		cfg.NodeName = hostname()
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return cfg, nil
}

// list splits a comma separated variable, trimming each entry and a
// trailing slash on a URL.
func list(raw string) []string {
	var out []string
	for s := range strings.SplitSeq(raw, ",") {
		if s = strings.TrimRight(strings.TrimSpace(s), "/"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// checkIssuer is spec 007's issuer scheme rule: https, or http on a
// loopback host or when the URL is listed in ORIGO_OIDC_INSECURE_ISSUERS.
func checkIssuer(iss string, insecure []string) string {
	u, err := url.Parse(iss)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Sprintf("ORIGO_OIDC_ISSUERS: %s is not an absolute http or https URL", iss)
	}
	if u.Scheme == "https" || slices.Contains(insecure, iss) {
		return ""
	}
	host := u.Hostname()
	if host == "localhost" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("ORIGO_OIDC_ISSUERS: %s uses http:// on a host that is not a loopback address; use https:// or list it in ORIGO_OIDC_INSECURE_ISSUERS", iss)
}

// Resolve fills the values that depend on the machine: it creates DataDir
// and derives CacheBytes from the disk when it was not set. It is separate
// from Load so a configuration is validated before anything touches the
// disk.
func (c *Config) Resolve() error {
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return fmt.Errorf("ORIGO_DATA_DIR: %w", err)
	}
	if c.CacheBytes == 0 {
		size, err := diskSize(c.DataDir)
		if err != nil {
			return fmt.Errorf("ORIGO_DATA_DIR: %w", err)
		}
		c.CacheBytes = size / 10 * 8
	}
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func duration(getenv Getenv, key string, def time.Duration, problems *[]string) time.Duration {
	raw := getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		*problems = append(*problems, key+" must be a duration such as 10m")
		return def
	}
	return d
}

// osHostname is a variable so a test covers the fallback.
var osHostname = os.Hostname

func hostname() string {
	h, err := osHostname()
	if err != nil || h == "" {
		return "origod"
	}
	return h
}
