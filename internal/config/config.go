// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package config reads the typed configuration of origod from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/hostmatch"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/limits"
	"github.com/latere-ai/origo/internal/sshd"
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
	// runs at once (spec 012). RequestsPerMinute is
	// ORIGO_REQUESTS_PER_MINUTE, the requests one effective subject may
	// send this node in a minute; 0 turns the per-subject limit off.
	MaxGitProcs       int
	RequestsPerMinute int

	// AnonymousRead is ORIGO_ANONYMOUS_READ (spec 027): when set, a
	// request that carries no credential on one of the read routes that
	// spec names is admitted with an empty subject and decided by the
	// authorizer like any other. Off by default, and an installation that
	// leaves it off behaves exactly as it did before that spec.
	// AnonymousRequestsPerMinute is
	// ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE, the refill of the one bucket
	// every anonymous caller of this node shares; 0 turns that limit off,
	// which no installation should do.
	AnonymousRead              bool
	AnonymousRequestsPerMinute int

	// The egress rules of spec 016 for a server-side fetch (import,
	// verify). EgressAllow is ORIGO_EGRESS_ALLOW: the hosts a fetch may
	// reach, exact names or *. wildcards, lower-cased with a trailing
	// dot removed; empty refuses every source. EgressPinned holds the
	// address an exact host of the list was pinned to as host=address,
	// the one form that admits a source inside the cluster. ClusterCIDRs
	// is ORIGO_CLUSTER_CIDRS, the service and pod ranges a fetch must
	// never reach beside the well-known refused ranges. EgressCA is the
	// system roots plus ORIGO_EGRESS_CA_BUNDLE, the certificates the
	// egress proxy trusts when it dials a source; nil when the variable
	// is unset, which means the system roots alone.
	EgressAllow  []string
	EgressPinned map[string]netip.Addr
	ClusterCIDRs []netip.Prefix
	EgressCA     *x509.CertPool

	// SSH (spec 024). SSHAddr is ORIGO_SSH_ADDR, the address the SSH
	// listener binds; empty turns SSH off and the node runs as it does
	// without it. The other three are required with it: SSHHostKeys is
	// the ordered set of ORIGO_SSH_HOST_KEYS, read from files at
	// start-up and never from the bucket, and SSHKeysURL with
	// SSHKeysToken is the operator's key resolution endpoint and the
	// bearer Origo presents it.
	SSHAddr      string
	SSHHostKeys  *sshd.HostKeys
	SSHKeysURL   string
	SSHKeysToken string

	// Failpoint names an injected failure for the end-to-end suite, for
	// example "commit.before-index". Empty in every deployment.
	Failpoint string
	// DropCapability is ORIGO_TEST_DROP_CAPABILITY (spec 021): one
	// git-controlled capability of spec 003's table the node stops
	// advertising, for the mutation job that proves the conformance
	// suite notices. One of DropCapabilities; empty in every deployment.
	DropCapability string
}

// DropCapabilities is the set ORIGO_TEST_DROP_CAPABILITY takes its value
// from: the capabilities the node controls through the repository
// configuration internal/repo writes. shallow, deepen-since, deepen-not,
// and report-status-v2 are advertised by git whatever the configuration
// and are not in the set.
var DropCapabilities = []string{"filter", "allow-tip-sha1-in-want", "allow-reachable-sha1-in-want", "atomic", "push-options"}

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
		DropCapability:  getenv("ORIGO_TEST_DROP_CAPABILITY"),
	}
	if cfg.DropCapability != "" && !slices.Contains(DropCapabilities, cfg.DropCapability) {
		problems = append(problems, "ORIGO_TEST_DROP_CAPABILITY must be one of "+strings.Join(DropCapabilities, ", "))
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
	cfg.RequestsPerMinute = limits.RequestsPerMinute
	if raw := getenv("ORIGO_REQUESTS_PER_MINUTE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			problems = append(problems, "ORIGO_REQUESTS_PER_MINUTE must be a non-negative integer")
		} else {
			cfg.RequestsPerMinute = n
		}
	}
	cfg.AnonymousRead = getenv("ORIGO_ANONYMOUS_READ") == "1"
	cfg.AnonymousRequestsPerMinute = limits.AnonymousRequestsPerMinute
	if raw := getenv("ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			problems = append(problems, "ORIGO_ANONYMOUS_REQUESTS_PER_MINUTE must be a non-negative integer")
		} else {
			cfg.AnonymousRequestsPerMinute = n
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
	sshConfig(getenv, cfg, &problems)
	cfg.EgressAllow, cfg.EgressPinned = egressAllow(getenv("ORIGO_EGRESS_ALLOW"), &problems)
	for _, raw := range list(getenv("ORIGO_CLUSTER_CIDRS")) {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("ORIGO_CLUSTER_CIDRS: %s is not a CIDR range", raw))
			continue
		}
		cfg.ClusterCIDRs = append(cfg.ClusterCIDRs, p.Masked())
	}
	if path := getenv("ORIGO_EGRESS_CA_BUNDLE"); path != "" {
		pool, err := caBundle(path)
		if err != nil {
			problems = append(problems, "ORIGO_EGRESS_CA_BUNDLE: "+err.Error())
		}
		cfg.EgressCA = pool
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

// sshConfig reads the four variables of spec 024. They are all or
// nothing: ORIGO_SSH_ADDR unset opens no listener and the other three
// are not read, and ORIGO_SSH_ADDR set requires all three, because a
// listener with no host key or no key resolver serves nobody and would
// fail on its first connection rather than at start-up.
func sshConfig(getenv Getenv, cfg *Config, problems *[]string) {
	cfg.SSHAddr = getenv("ORIGO_SSH_ADDR")
	if cfg.SSHAddr == "" {
		return
	}
	paths := list(getenv("ORIGO_SSH_HOST_KEYS"))
	switch {
	case len(paths) == 0:
		*problems = append(*problems, "missing ORIGO_SSH_HOST_KEYS")
	default:
		keys, err := sshd.ParseHostKeys(paths)
		if err != nil {
			*problems = append(*problems, "ORIGO_SSH_HOST_KEYS: "+err.Error())
		}
		cfg.SSHHostKeys = keys
	}
	if raw := getenv("ORIGO_SSH_KEYS_URL"); raw == "" {
		*problems = append(*problems, "missing ORIGO_SSH_KEYS_URL")
	} else if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		*problems = append(*problems, "ORIGO_SSH_KEYS_URL must be an absolute http or https URL")
	} else {
		cfg.SSHKeysURL = raw
	}
	if cfg.SSHKeysToken = getenv("ORIGO_SSH_KEYS_TOKEN"); cfg.SSHKeysToken == "" {
		*problems = append(*problems, "missing ORIGO_SSH_KEYS_TOKEN")
	}
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

// NormalizeHost is the host normalization of the egress rules (spec
// 016): lower case, no trailing dot, no surrounding space. The list
// and every host checked against it go through it.
func NormalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// egressAllow parses ORIGO_EGRESS_ALLOW: comma separated hosts, exact
// or *. wildcards, an exact host optionally pinned to one IP literal as
// host=address. A pinned address on a wildcard, one that is not an IP
// literal, and a host that is neither form are problems.
func egressAllow(raw string, problems *[]string) ([]string, map[string]netip.Addr) {
	var hosts []string
	pinned := map[string]netip.Addr{}
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host, address, hasPin := strings.Cut(entry, "=")
		host = NormalizeHost(host)
		if !hostmatch.ValidPattern(host) {
			*problems = append(*problems, fmt.Sprintf("ORIGO_EGRESS_ALLOW: %s is not a hostname or a *. wildcard", entry))
			continue
		}
		if hasPin {
			addr, err := netip.ParseAddr(strings.TrimSpace(address))
			switch {
			case strings.HasPrefix(host, "*."):
				*problems = append(*problems, fmt.Sprintf("ORIGO_EGRESS_ALLOW: %s pins an address on a wildcard, which only an exact host may carry", entry))
				continue
			case err != nil:
				*problems = append(*problems, fmt.Sprintf("ORIGO_EGRESS_ALLOW: %s pins %q, which is not an IP literal", entry, address))
				continue
			}
			pinned[host] = addr.Unmap()
		}
		hosts = append(hosts, host)
	}
	return hosts, pinned
}

// caBundle reads a PEM file of certificates and returns the system roots
// with them appended.
func caBundle(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no certificate", path)
	}
	return pool, nil
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
