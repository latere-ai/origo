// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latere-ai/origo/internal/api"
	"github.com/latere-ai/origo/internal/config"
	versionpkg "github.com/latere-ai/origo/internal/version"
	"github.com/latere-ai/origo/internal/wal"
)

// The batch migration of spec 014: origod migrate reads a manifest of
// repositories, drives each one from registered to mirrored against an
// Origo, and writes a report line per repository as it finishes. It is
// a client of the API and not a node: it reads three variables of its
// own and none of the node's, opens no bucket, and holds no state, so
// a second run resumes from what Origo answers and nothing else.

// The states a report line carries.
const (
	stateMirrored = "mirrored"
	stateSkipped  = "skipped"
	stateFailed   = "failed"
)

// The poll of an import that is still running: the first ask comes
// quickly, so a small repository costs no wait, and the interval
// doubles to pollMax, so a large one costs few requests.
const (
	pollMin = 100 * time.Millisecond
	pollMax = 5 * time.Second
	// callTimeout bounds one request to Origo. An import answers 202 at
	// once and a verify runs inside the node's read budget, so no call
	// of this command is long.
	callTimeout = 2 * time.Minute
	// driveTimeout bounds one repository's whole run, the import's own
	// budget of spec 019 with room for the register and the verify.
	driveTimeout = 45 * time.Minute
)

// migrateOptions is the three variables of spec 014's table.
type migrateOptions struct {
	// URL is ORIGO_MIGRATE_URL, the Origo the command drives.
	URL string
	// Token is the bearer read out of the variable
	// ORIGO_MIGRATE_TOKEN_ENV names, never a value on the command line
	// and never one in the manifest.
	Token string
	// Parallel is ORIGO_MIGRATE_PARALLEL, repositories driven at once.
	Parallel int
}

// loadMigrateOptions reads the command's own configuration, collecting
// every problem into one message the way the node's Load does.
func loadMigrateOptions(getenv config.Getenv) (migrateOptions, error) {
	var o migrateOptions
	var problems []string
	o.URL = strings.TrimRight(strings.TrimSpace(getenv("ORIGO_MIGRATE_URL")), "/")
	switch u, err := url.Parse(o.URL); {
	case o.URL == "":
		problems = append(problems, "missing ORIGO_MIGRATE_URL")
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		problems = append(problems, "ORIGO_MIGRATE_URL: an absolute http or https URL")
	}
	name := strings.TrimSpace(getenv("ORIGO_MIGRATE_TOKEN_ENV"))
	switch {
	case name == "":
		problems = append(problems, "missing ORIGO_MIGRATE_TOKEN_ENV")
	default:
		o.Token = getenv(name)
		if o.Token == "" {
			problems = append(problems, "ORIGO_MIGRATE_TOKEN_ENV names "+name+", which is empty")
		}
	}
	o.Parallel = 4
	if raw := strings.TrimSpace(getenv("ORIGO_MIGRATE_PARALLEL")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			problems = append(problems, "ORIGO_MIGRATE_PARALLEL: a positive whole number")
		} else {
			o.Parallel = n
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return o, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return o, nil
}

// manifestLine is one repository of the manifest. prior_id is the id
// the prior host holds it under; Origo never reads it and the report
// copies it back, so the mapping between the two lives in the prior
// host and in these two files and nowhere else.
type manifestLine struct {
	ID       string `json:"id"`
	PriorID  string `json:"prior_id,omitempty"`
	Owner    string `json:"owner"`
	Slug     string `json:"slug"`
	Source   string `json:"source"`
	TokenEnv string `json:"token_env,omitempty"`

	// token is the bearer read out of the variable TokenEnv names.
	token string
}

// reportLine is one repository of the report, written when it finishes.
type reportLine struct {
	ID      string  `json:"id"`
	PriorID string  `json:"prior_id"`
	Owner   string  `json:"owner"`
	Slug    string  `json:"slug"`
	State   string  `json:"state"`
	Refs    int     `json:"refs"`
	Objects int64   `json:"objects"`
	Seconds float64 `json:"seconds"`
	Error   string  `json:"error"`
}

// readManifest parses and validates the whole manifest before anything
// is called: a line Origo would refuse is refused here, so a batch that
// starts is a batch whose every line is addressable.
func readManifest(path string, getenv config.Getenv) ([]manifestLine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []manifestLine
	seen := map[string]int{}
	for n, raw := range strings.Split(string(data), "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var l manifestLine
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		if err := l.validate(getenv); err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		if first, ok := seen[l.ID]; ok {
			return nil, fmt.Errorf("line %d: id %s is also on line %d", n+1, l.ID, first)
		}
		seen[l.ID] = n + 1
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return nil, errors.New("the manifest names no repository")
	}
	return lines, nil
}

// validate holds one line to what Origo accepts and reads its bearer.
func (l *manifestLine) validate(getenv config.Getenv) error {
	if !wal.ValidID(l.ID) {
		return fmt.Errorf("id %q is not a lower-case UUID", l.ID)
	}
	if !wal.ValidLabel(l.Owner) {
		return fmt.Errorf("owner %q is not a URL-safe label", l.Owner)
	}
	if !wal.ValidLabel(l.Slug) {
		return fmt.Errorf("slug %q is not a URL-safe label", l.Slug)
	}
	u, err := url.Parse(l.Source)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("source %q is not an absolute https URL", l.Source)
	}
	if l.TokenEnv != "" {
		if l.token = getenv(l.TokenEnv); l.token == "" {
			return fmt.Errorf("token_env names %s, which is empty", l.TokenEnv)
		}
	}
	return nil
}

// reporter writes the report, one JSON line per repository in finishing
// order. Every worker writes through it, so it serializes.
type reporter struct {
	mu sync.Mutex
	w  io.Writer
}

func (r *reporter) write(line reportLine) error {
	raw, err := json.Marshal(line)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.w.Write(append(raw, '\n'))
	return err
}

// migrate is the subcommand.
func migrate(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("origod migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifest := fs.String("manifest", "", "the JSON lines file naming every repository to migrate")
	report := fs.String("report", "", "the JSON lines file the result of each repository is written to")
	showVersion := versionFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, versionpkg.String())
		return 0
	}
	if *manifest == "" || *report == "" {
		_, _ = fmt.Fprintln(stderr, "usage: origod migrate -manifest <file> -report <file>")
		return 2
	}
	o, err := loadMigrateOptions(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "origod:", err)
		return 1
	}
	lines, err := readManifest(*manifest, getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "origod: %s: %v\n", *manifest, err)
		return 2
	}
	out, err := os.Create(*report)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "origod:", err)
		return 1
	}
	defer func() { _ = out.Close() }()
	c := &migrateClient{options: o, http: &http.Client{Transport: &http.Transport{}}}
	failed := c.run(ctx, lines, &reporter{w: out})
	_, _ = fmt.Fprintf(stdout, "migrated %d of %d repositories; the report is %s\n", len(lines)-failed, len(lines), *report)
	if failed > 0 {
		return 1
	}
	return 0
}

// migrateClient drives repositories against one Origo.
type migrateClient struct {
	options migrateOptions
	http    *http.Client
}

// run drives every line with the configured parallelism and answers how
// many finished failed.
func (c *migrateClient) run(ctx context.Context, lines []manifestLine, r *reporter) int {
	work := make(chan manifestLine)
	var failed atomic.Int64
	var wg sync.WaitGroup
	for range min(c.options.Parallel, len(lines)) {
		wg.Go(func() {
			for l := range work {
				line := c.drive(ctx, l)
				if line.State == stateFailed {
					failed.Add(1)
				}
				if err := r.write(line); err != nil {
					failed.Add(1)
				}
			}
		})
	}
	for _, l := range lines {
		work <- l
	}
	close(work)
	wg.Wait()
	return int(failed.Load())
}

// drive takes one repository from registered to mirrored, resuming from
// what Origo answers: a repository it does not hold is registered, an
// import that has not run is started, and a repository already verified
// equal is skipped without importing again.
func (c *migrateClient) drive(ctx context.Context, l manifestLine) reportLine {
	started := time.Now()
	line := reportLine{ID: l.ID, PriorID: l.PriorID, Owner: l.Owner, Slug: l.Slug}
	ctx, cancel := context.WithTimeout(ctx, driveTimeout)
	defer cancel()
	state, refs, objects, err := c.mirror(ctx, l)
	line.State, line.Refs, line.Objects = state, refs, objects
	if err != nil {
		line.Error = err.Error()
	}
	line.Seconds = float64(time.Since(started).Round(time.Millisecond)) / float64(time.Second)
	return line
}

// mirror is the phases of spec 014 for one repository.
func (c *migrateClient) mirror(ctx context.Context, l manifestLine) (string, int, int64, error) {
	repo, err := c.repository(ctx, l)
	if err != nil {
		return stateFailed, 0, 0, err
	}
	// Already verified equal: the batch resumes and reports it skipped
	// without touching either side again.
	if repo.VerifiedAt != nil && repo.VerifiedEqual != nil && *repo.VerifiedEqual {
		return stateSkipped, 0, 0, nil
	}
	if err := c.importOnce(ctx, l); err != nil {
		return stateFailed, 0, 0, err
	}
	result, err := c.verify(ctx, l)
	if err != nil {
		return stateFailed, 0, 0, err
	}
	if !result.Equal {
		return stateFailed, result.Refs.Origo, result.Objects.Origo, fmt.Errorf("verify: the two sides differ on %d references, the first %s", len(result.Refs.Differing), first(result.Refs.Differing))
	}
	return stateMirrored, result.Refs.Origo, result.Objects.Origo, nil
}

// first names a differing reference for the report's error field.
func first(differing []api.RefDifference) string {
	if len(differing) == 0 {
		return "none"
	}
	return differing[0].Name
}

// repository reads the repository, registering it when Origo does not
// hold it yet.
func (c *migrateClient) repository(ctx context.Context, l manifestLine) (api.Repository, error) {
	var repo api.Repository
	status, body, err := c.call(ctx, "GET", "/v1/repos/"+l.ID, nil)
	if err != nil {
		return repo, fmt.Errorf("get: %w", err)
	}
	switch status {
	case http.StatusOK:
		return repo, decodeInto(body, &repo, "get")
	case http.StatusNotFound:
	default:
		return repo, refusal("get", status, body)
	}
	status, body, err = c.call(ctx, "POST", "/v1/repos", map[string]any{"id": l.ID, "owner": l.Owner, "slug": l.Slug})
	if err != nil {
		return repo, fmt.Errorf("create: %w", err)
	}
	if status != http.StatusCreated {
		return repo, refusal("create", status, body)
	}
	return repo, decodeInto(body, &repo, "create")
}

// importOnce starts the import unless one already ran, and waits for it
// either way.
func (c *migrateClient) importOnce(ctx context.Context, l manifestLine) error {
	st, err := c.importState(ctx, l)
	if err != nil {
		return err
	}
	if st.State == api.ImportDone {
		return nil
	}
	if st.State != api.ImportRunning {
		status, body, err := c.call(ctx, "POST", "/v1/repos/"+l.ID+"/import", map[string]any{"source": l.Source, "token": l.token})
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		if status != http.StatusAccepted {
			return refusal("import", status, body)
		}
	}
	wait := pollMin
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("import: %w", ctx.Err())
		case <-time.After(wait):
		}
		if wait *= 2; wait > pollMax {
			wait = pollMax
		}
		st, err := c.importState(ctx, l)
		if err != nil {
			return err
		}
		switch st.State {
		case api.ImportDone:
			return nil
		case api.ImportFailed:
			return fmt.Errorf("import: %s", st.Error)
		}
	}
}

// importState reads the import of one repository; a repository no
// import ran for answers 404 import_not_found, which is a state and not
// a failure.
func (c *migrateClient) importState(ctx context.Context, l manifestLine) (api.ImportState, error) {
	var st api.ImportState
	status, body, err := c.call(ctx, "GET", "/v1/repos/"+l.ID+"/import", nil)
	if err != nil {
		return st, fmt.Errorf("import: %w", err)
	}
	switch status {
	case http.StatusOK:
		return st, decodeInto(body, &st, "import")
	case http.StatusNotFound:
		return api.ImportState{}, nil
	default:
		return st, refusal("import", status, body)
	}
}

// verify compares the source with Origo's copy.
func (c *migrateClient) verify(ctx context.Context, l manifestLine) (api.VerifyResult, error) {
	var res api.VerifyResult
	status, body, err := c.call(ctx, "POST", "/v1/repos/"+l.ID+"/verify", map[string]any{"source": l.Source, "token": l.token})
	if err != nil {
		return res, fmt.Errorf("verify: %w", err)
	}
	if status != http.StatusOK {
		return res, refusal("verify", status, body)
	}
	return res, decodeInto(body, &res, "verify")
}

// call is one request to Origo with the bearer, the body encoded when
// there is one. The bearer never reaches an argument or a URL.
func (c *migrateClient) call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.options.URL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.options.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}

func decodeInto(body []byte, v any, step string) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	return nil
}

// refusal is the error a report line carries for a step Origo refused:
// the step, Origo's error code, and the developer reason when the
// envelope names one.
func refusal(step string, status int, body []byte) error {
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	code := env.Error.Code
	if code == "" {
		code = "http " + strconv.Itoa(status)
	}
	if reason, ok := env.Error.Details["reason"].(string); ok && reason != "" {
		return fmt.Errorf("%s: %s: %s", step, code, reason)
	}
	return fmt.Errorf("%s: %s", step, code)
}
