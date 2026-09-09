// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/config"
	"github.com/latere-ai/origo/internal/events"
	versionpkg "github.com/latere-ai/origo/internal/version"
	"github.com/latere-ai/origo/internal/wal"
)

// `origod check` reads the same configuration as `origod serve` and
// reaches everything the node depends on: the bucket and its conditional
// create, the issuers, the authorization endpoint, the event sink, the
// disk, and git. It prints one line per requirement and exits 1 on any
// failure, so it runs as an init container in front of the node and as
// the first thing an operator runs after an install.
//
// Nothing here is a substitute for the node's own start-up: a value that
// is missing or malformed fails config.Load before any of this runs.
// These are the answers only the outside world can give.

// The probe identity the authorizer requirement uses: an empty subject
// and the repository id the authorizer contract reserves, which every
// authorizer must deny. A deployment whose endpoint allows it would
// allow anything.
const (
	probeRepoID  = "00000000-0000-0000-0000-000000000001"
	probeSubject = ""
)

// gitFloor is the oldest git the node's operations are written against.
// The released image carries a newer one; the floor is what the features
// need, not what the image ships.
var gitFloor = [3]int{2, 40, 0}

// checkTimeout bounds one requirement. A requirement that hangs is a
// failure of that requirement and never of the whole command, so the
// operator sees the other six.
const checkTimeout = 20 * time.Second

// requirement is one line of the report.
type requirement struct {
	name   string
	ok     bool
	detail string
}

// line renders the requirement as the report writes it: `ok <name>`,
// `ok <name>: <detail>` where an answer needs a word, or
// `fail <name>: <detail>`.
func (r requirement) line() string {
	switch {
	case r.ok && r.detail == "":
		return "ok " + r.name
	case r.ok:
		return "ok " + r.name + ": " + r.detail
	default:
		return "fail " + r.name + ": " + r.detail
	}
}

func ok(name string) requirement { return requirement{name: name, ok: true} }
func okWith(name, detail string) requirement {
	return requirement{name: name, ok: true, detail: detail}
}
func fail(name string, detail any) requirement {
	return requirement{name: name, detail: fmt.Sprint(detail)}
}
func failf(name, f string, a ...any) requirement { return fail(name, fmt.Sprintf(f, a...)) }

// check is the subcommand: load the configuration the node loads, run
// every requirement, print the report, and exit 1 when one failed.
func check(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("origod check", flag.ContinueOnError)
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
	code := 0
	for _, r := range requirements(ctx, cfg, getenv) {
		_, _ = fmt.Fprintln(stdout, r.line())
		if !r.ok {
			code = 1
		}
	}
	return code
}

// requirements runs the seven in the order the report prints them. The
// count never changes: a requirement that does not apply to this
// installation says so on its own line.
func requirements(ctx context.Context, cfg *config.Config, getenv config.Getenv) []requirement {
	client := &http.Client{Transport: storageTransport()}
	store, storeErr := wal.NewS3(wal.S3Options{
		Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		Key: cfg.S3Key, Secret: cfg.S3Secret, PathStyle: cfg.S3PathStyle, Client: client,
	})
	var bucket, conditional requirement
	if storeErr != nil {
		bucket, conditional = fail("bucket", storeErr), fail("conditional-create", storeErr)
	} else {
		bucket = checkBucket(ctx, store)
		conditional = checkConditionalCreate(ctx, store, cfg, getenv, client)
	}
	return []requirement{
		bucket,
		conditional,
		checkIssuers(ctx, cfg, client),
		checkAuthorizer(ctx, cfg, client),
		checkEvents(ctx, cfg, client),
		checkDisk(cfg),
		checkGit(ctx),
	}
}

// checkBucket lists one key under the prefix. It proves the endpoint,
// the region, the credentials, and the bucket name in one call, and
// reads nothing.
func checkBucket(ctx context.Context, store wal.Store) requirement {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if _, err := store.List(ctx, wal.ListOptions{Prefix: config.Prefix, Max: 1}); err != nil {
		return fail("bucket", err)
	}
	return ok("bucket")
}

// checkConditionalCreate is the one requirement a bucket can meet or
// miss silently: the log is linearized by a create that fails when the
// key is present, so a store that answers the second create with a 200
// would let two nodes commit the same sequence. It writes one key and
// removes it.
//
// With ORIGO_CHECK_SELFTEST=1 the two creates run against a store inside
// this process that accepts every write and ignores the header, which is
// how the detection itself is proved: against a conforming store the
// second create is always refused.
func checkConditionalCreate(ctx context.Context, store wal.Store, cfg *config.Config, getenv config.Getenv, client *http.Client) requirement {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if getenv("ORIGO_CHECK_SELFTEST") == "1" {
		selftest, stop, err := permissiveStore(ctx, cfg, client)
		if err != nil {
			return fail("conditional-create", err)
		}
		defer stop()
		store = selftest
	}
	key := config.Prefix + "check/" + uuid.NewString()
	body := []byte("origo check\n")
	if _, err := store.Create(ctx, key, wal.BytesBody(body)); err != nil {
		return failf("conditional-create", "the first create failed: %v", err)
	}
	defer func() { _ = store.Delete(context.WithoutCancel(ctx), key) }()
	switch _, err := store.Create(ctx, key, wal.BytesBody(body)); {
	case errors.Is(err, wal.ErrExists):
		return ok("conditional-create")
	case err != nil:
		return failf("conditional-create", "the second create failed with %v, not a refusal", err)
	default:
		return fail("conditional-create", "second create answered 200")
	}
}

// permissiveStore is the in-process store of ORIGO_CHECK_SELFTEST: it
// accepts every PUT and ignores If-None-Match, the way a store that
// cannot linearize writers behaves. It answers on the loopback interface
// and stops with the requirement.
func permissiveStore(ctx context.Context, cfg *config.Config, client *http.Client) (wal.Store, func(), error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		ReadHeaderTimeout: readHeaderTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				w.Header().Set("ETag", `"selftest"`)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = srv.Serve(listener) }()
	stop := func() { _ = srv.Close() }
	store, err := wal.NewS3(wal.S3Options{
		Endpoint: "http://" + listener.Addr().String(), Region: cfg.S3Region, Bucket: cfg.S3Bucket,
		Key: cfg.S3Key, Secret: cfg.S3Secret, PathStyle: true, Client: client,
	})
	if err != nil {
		stop()
		return nil, nil, err
	}
	return store, stop, nil
}

// checkIssuers fetches every issuer's discovery document and key set
// through the same call the node makes. A token cannot be verified
// before those two documents are read, so an issuer that does not answer
// here is one no client can authenticate against.
func checkIssuers(ctx context.Context, cfg *config.Config, client *http.Client) requirement {
	for _, iss := range cfg.OIDCIssuers {
		keys, err := auth.FetchKeys(ctx, client, iss, checkTimeout)
		switch {
		case err != nil:
			return failf("issuer", "%s: %v", iss, err)
		case len(keys) == 0:
			return failf("issuer", "%s: the key set is empty", iss)
		}
	}
	return ok("issuer")
}

// checkAuthorizer asks the endpoint about the reserved probe id with an
// empty subject. The contract requires a deny, so an allow says the
// endpoint answers without reading the request, which would let any
// subject reach any repository.
func checkAuthorizer(ctx context.Context, cfg *config.Config, client *http.Client) requirement {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	c, err := auth.NewClient(auth.ClientOptions{URL: cfg.AuthorizerURL, Token: cfg.AuthorizerToken, HTTP: client, Timeout: checkTimeout})
	if err != nil {
		return fail("authorizer", err)
	}
	decision, err := c.Authorize(ctx, auth.Request{Subject: probeSubject, Repo: auth.RepoRef{ID: probeRepoID}, Action: auth.ActionRead})
	switch {
	case err != nil:
		return fail("authorizer", err)
	case decision.Allow:
		return fail("authorizer", "probe id allowed")
	}
	return ok("authorizer")
}

// checkEvents delivers a ping to the sink, signed the way every event is
// signed, so a sink that verifies signatures answers it and one that
// rejects the key does not. Any status below 500 is an answer: the sink
// received it and decided, which is all a delivery needs to prove.
func checkEvents(ctx context.Context, cfg *config.Config, client *http.Client) requirement {
	if cfg.EventsURL == "" {
		return okWith("events", "not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	id := uuid.NewString()
	body, err := json.Marshal(map[string]any{"id": id, "kind": "ping", "at": time.Now().UTC()})
	if err != nil {
		return fail("events", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.EventsURL, strings.NewReader(string(body)))
	if err != nil {
		return fail("events", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(events.HeaderEvent, "ping")
	req.Header.Set(events.HeaderDelivery, id)
	req.Header.Set(events.HeaderSignature, events.Sign(cfg.EventsSecret, body))
	resp, err := client.Do(req)
	if err != nil {
		return fail("events", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= http.StatusInternalServerError {
		return failf("events", "the sink answered %d", resp.StatusCode)
	}
	return ok("events")
}

// checkDisk writes and removes a file where the repository cache lives
// and reports whether the file system is large enough for the cache
// ceiling. A cache ceiling above the disk fills it.
func checkDisk(cfg *config.Config) requirement {
	if err := cfg.Resolve(); err != nil {
		return fail("disk", err)
	}
	probe := filepath.Join(cfg.DataDir, ".origo-check")
	if err := os.WriteFile(probe, []byte("origo check\n"), 0o600); err != nil {
		return fail("disk", err)
	}
	if err := os.Remove(probe); err != nil {
		return fail("disk", err)
	}
	size, err := config.DiskSize(cfg.DataDir)
	if err != nil {
		return fail("disk", err)
	}
	if size < cfg.CacheBytes {
		return failf("disk", "%s holds %d bytes and ORIGO_CACHE_BYTES is %d", cfg.DataDir, size, cfg.CacheBytes)
	}
	return ok("disk")
}

// checkGit runs the git the node runs and reads its version. Every
// repository operation is a git subprocess, so a missing or old binary
// is a node that starts and then fails every request.
func checkGit(ctx context.Context) requirement {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return fail("git", err)
	}
	version, got, err := gitVersion(string(out))
	if err != nil {
		return fail("git", err)
	}
	if got[0] < gitFloor[0] || (got[0] == gitFloor[0] && got[1] < gitFloor[1]) {
		return failf("git", "%s is below %d.%d", version, gitFloor[0], gitFloor[1])
	}
	return okWith("git", version)
}

// gitVersion reads the version out of `git version 2.47.1`: the word
// after the second one, and the first three dot-separated numbers of it.
// A trailing word a distribution appends is ignored.
func gitVersion(out string) (string, [3]int, error) {
	var got [3]int
	fields := strings.Fields(out)
	if len(fields) < 3 {
		return "", got, fmt.Errorf("git --version answered %q", strings.TrimSpace(out))
	}
	version := fields[2]
	for i, part := range strings.SplitN(version, ".", 4) {
		if i == 3 {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return "", got, fmt.Errorf("git --version answered %q", strings.TrimSpace(out))
		}
		got[i] = n
	}
	return version, got, nil
}
