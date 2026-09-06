// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package config reads the typed configuration of origod from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Defaults for the optional variables.
const (
	DefaultDataDir       = "/var/lib/origo"
	DefaultPublicAddr    = ":8080"
	DefaultInternalAddr  = ":8081"
	DefaultGossipAddr    = ":7946"
	DefaultSweepInterval = 10 * time.Minute
	DefaultSweepMinAge   = time.Hour
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

	// DataDir holds the repository cache. It must be a local disk.
	DataDir string
	// CacheBytes is the eviction ceiling of the repository cache. Zero
	// means 80% of the disk that holds DataDir, resolved at start-up.
	CacheBytes int64

	// PublicURL is the origin clients see, used in clone URLs and event
	// payloads. Required.
	PublicURL *url.URL

	// DevToken is the phase 1 stand-in for authentication (spec 007): the
	// public listener accepts exactly this bearer and refuses everything
	// else. It is required until the OIDC issuers replace it.
	DevToken string

	// Spec 007 identity and authorization. Optional until spec 007 lands;
	// read now so a deployment that already sets them is not refused.
	OIDCIssuers     []string
	AuthorizerURL   string
	AuthorizerToken string

	// Spec 008 push events. Optional.
	EventsURL    string
	EventsSecret string

	// NodeName identifies the node in gossip and placement (spec 005). The
	// host name by default, which is the pod name in Kubernetes.
	NodeName string
	// GossipPeers is a DNS name resolving to every node. Optional.
	GossipPeers string

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
		DevToken:        missing("ORIGO_DEV_TOKEN"),
		AuthorizerURL:   getenv("ORIGO_AUTHORIZER_URL"),
		AuthorizerToken: getenv("ORIGO_AUTHORIZER_TOKEN"),
		EventsURL:       getenv("ORIGO_EVENTS_URL"),
		EventsSecret:    getenv("ORIGO_EVENTS_SECRET"),
		NodeName:        getenv("ORIGO_NODE_NAME"),
		GossipPeers:     getenv("ORIGO_GOSSIP_PEERS"),
		PublicAddr:      orDefault(getenv("ORIGO_PUBLIC_ADDR"), DefaultPublicAddr),
		InternalAddr:    orDefault(getenv("ORIGO_INTERNAL_ADDR"), DefaultInternalAddr),
		GossipAddr:      orDefault(getenv("ORIGO_GOSSIP_ADDR"), DefaultGossipAddr),
		Failpoint:       getenv("ORIGO_FAILPOINT"),
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
	if raw := getenv("ORIGO_OIDC_ISSUERS"); raw != "" {
		for s := range strings.SplitSeq(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.OIDCIssuers = append(cfg.OIDCIssuers, s)
			}
		}
	}
	if raw := getenv("ORIGO_CACHE_BYTES"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			problems = append(problems, "ORIGO_CACHE_BYTES must be a positive integer number of bytes")
		}
		cfg.CacheBytes = n
	}
	cfg.SweepInterval = duration(getenv, "ORIGO_SWEEP_INTERVAL", DefaultSweepInterval, &problems)
	cfg.SweepMinAge = duration(getenv, "ORIGO_SWEEP_MIN_AGE", DefaultSweepMinAge, &problems)
	if cfg.NodeName == "" {
		cfg.NodeName = hostname()
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return cfg, nil
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
