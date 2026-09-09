// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package conformance is the contract of spec 003 as executable tests
// (spec 021): Run drives the real git binary and net/http against any
// base URL, a live Origo, the contract stub of test/stubs/origo, or a
// consumer's own stub, and asserts every table of the contract and of
// the specs it points at, one subtest per row, named <spec>/<row>.
// Every repository the run creates carries a slug prefixed
// conformance- and is deleted at the end of the run by the ids it
// created, never by prefix, so a run against a shared installation
// leaves nothing and touches no other repository.
//
// Six groups of cases skip on their own when the field of Target they
// need is empty, each reported by name: the delegation group (Issuer),
// the deny-flipping group and the quota row (Authorizer), the source
// group (Source and SourceToken), and the two rows that need a Fault,
// storage_unavailable and repository_unavailable. Two codes no target
// can produce inside a run, gone and operation_timeout, are checked from
// the code table alone by TestEveryCodeHasOneSentence in
// internal/contract.
package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/contract"
	"github.com/latere-ai/origo/internal/gittest"
)

// Target is what a run drives.
type Target struct {
	// URL is the base URL of the target, ORIGO_PUBLIC_URL of a node.
	URL string
	// Token carries admin on every repository the run creates.
	Token string
	// Issuer is the stub issuer's URL, to mint tokens for the delegation
	// cases; empty on a live target.
	Issuer string
	// Authorizer is the stub authorizer's control URL, to flip allow and
	// deny and to set quota_bytes; empty on a live target.
	Authorizer string
	// EventsSink is the stub sink's control URL, to read deliveries;
	// empty on a live target, where the event cases assert the operation
	// and report the delivery unverified.
	EventsSink string
	// Source and SourceToken are a git source for the import cases of
	// spec 019 and the bearer it requires; empty on a live target and
	// on the stub run.
	Source      string
	SourceToken string
	// Fault cuts the bucket and deletes an object; nil on a live target.
	Fault Fault
	// Skip names subtests to skip, as <spec>/<row>; each is reported.
	Skip []string
}

// Fault is what the two degraded rows need and a caller outside the
// installation cannot cause. CutStorage makes the bucket unreachable
// until the test ends. DeleteObject deletes the one object under the
// key prefix and reports its key: the suite knows an entry's sequence
// and never its nonce, so the fault resolves the prefix.
type Fault interface {
	CutStorage(t testing.TB)
	DeleteObject(t testing.TB, prefix string) string
}

// The six groups that skip on their own, named as the live run's skip
// list names them.
const (
	GroupDelegation  = "delegation"
	GroupDeny        = "deny-flipping"
	GroupQuota       = "quota"
	GroupSource      = "source"
	GroupStorage     = "storage_unavailable"
	GroupRepository  = "repository_unavailable"
	groupEventsField = "EventsSink"
)

// Groups lists the six groups in the order of spec 021's table.
var Groups = []string{GroupDelegation, GroupDeny, GroupQuota, GroupSource, GroupStorage, GroupRepository}

// Report is what a run did: every case by outcome, the groups it
// skipped, the event deliveries it could not observe, and the ids it
// created and deleted.
type Report struct {
	Passed, Failed, Skipped []string
	SkippedGroups           []string
	Unverified              []string
	Created                 []string
}

// SlugPrefix is the prefix of every slug the run creates.
const SlugPrefix = "conformance-"

// Owner is the owner label of every repository the run creates.
const Owner = "conformance"

// testCase is one row: its name under the spec, the group that skips
// it, and the assertion.
type testCase struct {
	name  string
	group string
	run   func(t *testing.T, s *session)
}

// spec is one spec's rows.
type spec struct {
	number string
	cases  []testCase
}

// specs is every row, in run order: spec 012 last because its
// rate_limited case drains the subject's bucket, which every case after
// it would then meet.
func specs() []spec {
	return []spec{
		{"003", cases003()},
		{"007", cases007()},
		{"008", cases008()},
		{"009", cases009()},
		{"010", cases010()},
		{"015", cases015()},
		{"019", cases019()},
		{"012", cases012()},
	}
}

// session is one run's state: the target, the ids it created, and the
// fixture repository the read cases share.
type session struct {
	target Target
	client *http.Client

	mu         sync.Mutex
	created    []string
	unverified []string
	skipped    map[string]bool

	fixture *fixture
}

// Run drives the suite against the target and reports what it did.
// Every case is a subtest of t named <spec>/<row>.
func Run(t *testing.T, target Target) (report Report) {
	t.Helper()
	target.URL = strings.TrimRight(target.URL, "/")
	s := &session{target: target, client: &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Minute}, skipped: map[string]bool{}}
	defer func() { report.Created = s.cleanup(t) }()
	s.fixture = s.newFixture(t)
	for _, sp := range specs() {
		t.Run(sp.number, func(t *testing.T) {
			for _, c := range sp.cases {
				name := sp.number + "/" + c.name
				ok := t.Run(c.name, func(t *testing.T) {
					if slices.Contains(target.Skip, name) {
						s.skip(t, name, "skipped by the target's Skip list")
					}
					if c.group != "" && !s.has(c.group) {
						s.skip(t, name, "the "+c.group+" group needs "+groupField(c.group)+" on the target")
					}
					c.run(t, s)
				})
				switch {
				case s.skipped[name]:
					report.Skipped = append(report.Skipped, name)
					if c.group != "" && !s.has(c.group) && !slices.Contains(report.SkippedGroups, c.group) {
						report.SkippedGroups = append(report.SkippedGroups, c.group)
					}
				case ok:
					report.Passed = append(report.Passed, name)
				default:
					report.Failed = append(report.Failed, name)
				}
			}
		})
	}
	s.mu.Lock()
	report.Unverified = slices.Clone(s.unverified)
	s.mu.Unlock()
	return report
}

// skip records the skip under the case's name and ends the subtest
// with the reason, which is how a skipped case is reported by name.
func (s *session) skip(t *testing.T, name, reason string) {
	s.mu.Lock()
	s.skipped[name] = true
	s.mu.Unlock()
	t.Skipf("%s: %s", name, reason)
}

// has reports whether the target carries what a group needs.
func (s *session) has(group string) bool {
	switch group {
	case GroupDelegation:
		return s.target.Issuer != ""
	case GroupDeny, GroupQuota:
		return s.target.Authorizer != ""
	case GroupSource:
		return s.target.Source != "" && s.target.SourceToken != ""
	case GroupStorage, GroupRepository:
		return s.target.Fault != nil
	}
	return true
}

// groupField names the Target field a group needs.
func groupField(group string) string {
	switch group {
	case GroupDelegation:
		return "Issuer"
	case GroupDeny, GroupQuota:
		return "Authorizer"
	case GroupSource:
		return "Source and SourceToken"
	}
	return "Fault"
}

// unverifiable records an assertion the target gave no way to make,
// an event delivery without a sink to read.
func (s *session) unverifiable(t *testing.T, what string) {
	t.Helper()
	s.mu.Lock()
	s.unverified = append(s.unverified, t.Name()+": "+what)
	s.mu.Unlock()
	t.Logf("unverified on this target: %s (no EventsSink)", what)
}

// newID is a fresh lower-case UUID v4.
func newID(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// response is one HTTP answer with its body decoded when it is JSON.
type response struct {
	status int
	header http.Header
	body   []byte
	json   map[string]any
}

// code is the error code of an envelope body, "" when it is not one.
func (r response) code() string {
	e, _ := r.json["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// message is the sentence of an envelope body.
func (r response) message() string {
	e, _ := r.json["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

// details is the details object of an envelope body.
func (r response) details() map[string]any {
	e, _ := r.json["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

// request is one call of the surface.
type request struct {
	method, path, body, token string
	header                    map[string]string
}

// call sends a request with the target's token unless another is
// given, and decodes a JSON body.
func (s *session) call(t testing.TB, method, path, body string) response {
	t.Helper()
	return s.do(t, request{method: method, path: path, body: body, token: s.target.Token})
}

// as is call with another token, "" for none.
func (s *session) as(t testing.TB, token, method, path, body string) response {
	t.Helper()
	return s.do(t, request{method: method, path: path, body: body, token: token})
}

func (s *session) do(t testing.TB, r request) response {
	t.Helper()
	url := r.path
	if !strings.HasPrefix(url, "http") {
		url = s.target.URL + r.path
	}
	req, err := http.NewRequestWithContext(context.Background(), r.method, url, strings.NewReader(r.body))
	check(t, !(err != nil), "%v", err)
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	check(t, !(err != nil), "%s %s: %v", r.method, r.path, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, header: resp.Header, body: raw}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		_ = json.Unmarshal(raw, &out.json)
	}
	return out
}

// sentences is the code table as the suite compares responses to it:
// every code of internal/contract read once through its constant, so
// the table walk of TestEveryCodeHasOneSentence sees one call site per
// row here and no code the table lacks.
var sentences = map[string]string{
	contract.CodeInvalid:               contract.Sentence(contract.CodeInvalid),
	contract.CodeUnauthenticated:       contract.Sentence(contract.CodeUnauthenticated),
	contract.CodeForbidden:             contract.Sentence(contract.CodeForbidden),
	contract.CodeRepoNotFound:          contract.Sentence(contract.CodeRepoNotFound),
	contract.CodeRefNotFound:           contract.Sentence(contract.CodeRefNotFound),
	contract.CodeRepoExists:            contract.Sentence(contract.CodeRepoExists),
	contract.CodeNonFastForward:        contract.Sentence(contract.CodeNonFastForward),
	contract.CodeOverQuota:             contract.Sentence(contract.CodeOverQuota),
	contract.CodeRateLimited:           contract.Sentence(contract.CodeRateLimited),
	contract.CodeStorageUnavailable:    contract.Sentence(contract.CodeStorageUnavailable),
	contract.CodeAuthorizerUnavailable: contract.Sentence(contract.CodeAuthorizerUnavailable),
	contract.CodeBlobTooLarge:          contract.Sentence(contract.CodeBlobTooLarge),
	contract.CodeOperationTimeout:      contract.Sentence(contract.CodeOperationTimeout),
	contract.CodeLFSObjectMismatch:     contract.Sentence(contract.CodeLFSObjectMismatch),
	contract.CodeLFSObjectNotStored:    contract.Sentence(contract.CodeLFSObjectNotStored),
	contract.CodeLFSLocksUnsupported:   contract.Sentence(contract.CodeLFSLocksUnsupported),
	contract.CodeRepositoryUnavailable: contract.Sentence(contract.CodeRepositoryUnavailable),
	contract.CodeGone:                  contract.Sentence(contract.CodeGone),
	contract.CodeRepoFrozen:            contract.Sentence(contract.CodeRepoFrozen),
	contract.CodeRepoImporting:         contract.Sentence(contract.CodeRepoImporting),
	contract.CodeRepoNotEmpty:          contract.Sentence(contract.CodeRepoNotEmpty),
	contract.CodeImportNotFound:        contract.Sentence(contract.CodeImportNotFound),
	contract.CodeMergeConflict:         contract.Sentence(contract.CodeMergeConflict),
	contract.CodeInvalidChange:         contract.Sentence(contract.CodeInvalidChange),
}

// sentence is the table's sentence of a code; a code outside the table
// fails the test, the way contract.Sentence panics.
func sentence(t testing.TB, code string) string {
	t.Helper()
	s, ok := sentences[code]
	check(t, !(!ok), "no sentence for %s", code)
	return s
}

// expectError asserts an envelope of the code under its status with
// the table's sentence, and answers its details.
func expectError(t testing.TB, r response, status int, code string) map[string]any {
	t.Helper()
	check(t, !(r.status != status || r.code() != code || r.message() != sentence(t, code)), "want %d %s %q, got %d %s %q: %s", status, code, sentence(t, code), r.status, r.code(), r.message(), r.body)
	check(t, !(r.header.Get(contract.Header) != contract.Version), "%s %q on the refusal, want %q", contract.Header, r.header.Get(contract.Header), contract.Version)
	return r.details()
}

// expectStatus asserts a status and the contract header.
func expectStatus(t testing.TB, r response, status int) {
	t.Helper()
	check(t, !(r.status != status), "status %d, want %d: %s", r.status, status, r.body)
	check(t, !(r.header.Get(contract.Header) != contract.Version), "%s %q, want %q", contract.Header, r.header.Get(contract.Header), contract.Version)
}

// create makes a repository under the conformance owner with a fresh
// id and records it for the cleanup. label is the part of the slug
// after the prefix.
func (s *session) create(t testing.TB, label string) string {
	t.Helper()
	id := newID(t)
	r := s.call(t, "POST", "/v1/repos", fmt.Sprintf(`{"id":%q,"owner":%q,"slug":%q}`, id, Owner, SlugPrefix+label+"-"+id[:8]))
	s.record(id)
	expectStatus(t, r, http.StatusCreated)
	return id
}

// record remembers an id the run created.
func (s *session) record(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, id)
}

// cleanup deletes every repository the run created by id, waiting out
// a rate limit, and reports the ids.
func (s *session) cleanup(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	ids := slices.Clone(s.created)
	s.mu.Unlock()
	for _, id := range ids {
		for attempt := range 5 {
			r := s.call(t, "DELETE", "/v1/repos/"+id, "")
			if r.status != http.StatusTooManyRequests {
				if r.status != http.StatusAccepted && r.status != http.StatusNotFound && r.status != http.StatusGone {
					t.Errorf("cleanup of %s: %d %s", id, r.status, r.body)
				}
				break
			}
			if attempt == 4 {
				t.Errorf("cleanup of %s stayed rate limited", id)
			}
			time.Sleep(retryAfter(r.header))
		}
	}
	return ids
}

// retryAfter is the Retry-After header as a duration, a second when
// it is missing or malformed.
func retryAfter(h http.Header) time.Duration {
	if n, err := strconv.Atoi(h.Get("Retry-After")); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return time.Second
}

// repoURL is the id form of the clone URL with the token as git's
// basic auth password.
func (s *session) repoURL(id string) string { return s.repoURLAs(s.target.Token, id) }

func (s *session) repoURLAs(token, id string) string {
	return withCredential(s.target.URL, token) + "/r/" + id + ".git"
}

// nameURL is the label form of the clone URL.
func (s *session) nameURL(owner, slug string) string {
	return withCredential(s.target.URL, s.target.Token) + "/" + owner + "/" + slug + ".git"
}

func withCredential(base, token string) string {
	scheme, rest, ok := strings.Cut(base, "://")
	if !ok {
		return base
	}
	return scheme + "://x:" + token + "@" + rest
}

// git runs the client git in dir and answers its combined output.
func git(t testing.TB, dir string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gittest.Env(dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// mustGit runs git and fails the test on an error.
func mustGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := git(t, dir, args...)
	check(t, !(err != nil), "git %s: %v\n%s", strings.Join(args, " "), err, redact(out))
	return strings.TrimSpace(out)
}

// redact hides a credential in a clone URL git echoes back.
func redact(out string) string {
	var b strings.Builder
	for {
		i := strings.Index(out, "://x:")
		if i < 0 {
			break
		}
		j := strings.Index(out[i:], "@")
		if j < 0 {
			break
		}
		b.WriteString(out[:i] + "://x:***")
		out = out[i+j:]
	}
	b.WriteString(out)
	return b.String()
}

// clone clones url into a fresh directory.
func clone(t testing.TB, url string, args ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	mustGit(t, t.TempDir(), append(append([]string{"clone", "-q"}, args...), url, dir)...)
	return dir
}

// initRepo makes an empty working repository with origin set to url.
func initRepo(t testing.TB, url string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	mustGit(t, t.TempDir(), "init", "-q", "-b", "main", dir)
	mustGit(t, dir, "remote", "add", "origin", url)
	return dir
}

// commitFile writes a file and commits it, answering the commit id.
func commitFile(t testing.TB, dir, name string, content []byte, message string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "--", name)
	mustGit(t, dir, "commit", "-q", "-m", message)
	return mustGit(t, dir, "rev-parse", "HEAD")
}

// revList is the repository's whole history, one line per commit.
func revList(t testing.TB, dir string) string {
	t.Helper()
	return mustGit(t, dir, "rev-list", "--all", "--objects")
}

// str, num, and obj read a JSON value as a string, a number, or an
// object, the zero value when it is not one.
func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	n, _ := v.(float64)
	return n
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// mustJSON encodes v for a request body.
func mustJSON(t testing.TB, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	check(t, !(err != nil), "%v", err)
	return string(raw)
}

// check fails the test with the message unless ok holds: the one
// place every assertion of the suite fails through.
func check(t testing.TB, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

// waitFor polls cond until it holds or the budget ends.
func waitFor(t testing.TB, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %v", what, budget)
}

// fixture is the repository the read cases share: a history with a
// text file, a file in a directory, a binary file, an annotated tag,
// and a second commit on main.
type fixture struct {
	id, slug   string
	work       string
	c1, c2     string
	tag        string
	tagged     string
	binary     []byte
	blobA      string
	commitDate string
}

func (s *session) newFixture(t *testing.T) *fixture {
	t.Helper()
	id := s.create(t, "fixture")
	f := &fixture{id: id, slug: SlugPrefix + "fixture-" + id[:8]}
	f.work = clone(t, s.repoURL(id))
	f.binary = gittest.Bytes(4096, 7)
	f.c1 = commitFile(t, f.work, "a.txt", []byte("one\ntwo\n"), "first")
	f.blobA = mustGit(t, f.work, "rev-parse", "HEAD:a.txt")
	if err := os.WriteFile(filepath.Join(f.work, "bin.dat"), f.binary, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, f.work, "add", "bin.dat")
	mustGit(t, f.work, "commit", "-q", "-m", "binary")
	mustGit(t, f.work, "tag", "-a", "-m", "release one", "v1")
	f.tag = mustGit(t, f.work, "rev-parse", "v1")
	f.tagged = mustGit(t, f.work, "rev-parse", "v1^{commit}")
	f.c2 = commitFile(t, f.work, "sub/b.txt", []byte("three\n"), "second\n\nSigned-off-by: Conformance <c@example.com>")
	f.commitDate = mustGit(t, f.work, "log", "-1", "--format=%cI", f.c1)
	mustGit(t, f.work, "push", "-q", "origin", "HEAD:refs/heads/main", "refs/tags/v1")
	return f
}

// run runs a command in dir and answers its combined output.
func run(dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// decode reads a JSON body into v.
func decode(t testing.TB, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
}
