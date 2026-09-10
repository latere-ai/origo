// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latere-ai/origo/internal/gittest"
	"github.com/latere-ai/origo/test/stubs/authorizer"
)

var (
	origoOnce sync.Once
	origoBin  string
	origoErr  error
)

// origoCommand builds the agent client of spec 025 once per test process.
func origoCommand(t *testing.T) string {
	t.Helper()
	origoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "origo-e2e-cli-")
		if err != nil {
			origoErr = err
			return
		}
		origoBin = filepath.Join(dir, "origo")
		cmd := exec.CommandContext(context.Background(), "go", "build", "-o", origoBin, "../../cmd/origo")
		if out, err := cmd.CombinedOutput(); err != nil {
			origoErr = fmt.Errorf("build: %v: %s", err, out)
		}
	})
	if origoErr != nil {
		t.Fatal(origoErr)
	}
	return origoBin
}

// client is the command pointed at one node with one credential.
type client struct {
	t    *testing.T
	bin  string
	dir  string
	env  []string
	seen []string
}

// newClient mints a repository-bound token of the given scope and points the
// command at the node with it. The credential an agent gets is bound to one
// repository and expires within the hour (spec 007), which is what the write
// gate rests on.
func newClient(t *testing.T, n *node, id, scope string) *client {
	t.Helper()
	status, body := n.api("POST", "/v1/repos/"+id+"/tokens", fmt.Sprintf(`{"scope":%q,"ttl":3600}`, scope))
	if status != 201 {
		t.Fatalf("mint a %s token: %d %v", scope, status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("no token in %v", body)
	}
	return &client{
		t: t, bin: origoCommand(t), dir: t.TempDir(),
		env: []string{
			"ORIGO_URL=http://" + n.public,
			"ORIGO_TOKEN=" + token,
			"ORIGO_REPO=" + id,
			`ORIGO_AUTHOR=Ada Lovelace <ada@example.com>`,
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
		},
	}
}

// answer is one run of the command: what a pipeline reads, what a person
// reads, and the exit code.
type answer struct {
	stdout string
	stderr string
	code   int
}

func (c *client) run(args ...string) answer {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = c.dir
	cmd.Env = c.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exec returns it unwrapped
		code = exit.ExitCode()
	} else if err != nil {
		c.t.Fatalf("origo %v: %v", args, err)
	}
	c.seen = append(c.seen, "origo "+strings.Join(args, " "))
	return answer{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// ok runs a command that is expected to be served.
func (c *client) ok(args ...string) answer {
	c.t.Helper()
	got := c.run(args...)
	if got.code != 0 {
		c.t.Fatalf("origo %v exited %d: %s", args, got.code, got.stderr)
	}
	return got
}

// head reads the whole object id of the branch's newest commit, through the
// pipeline the skill documents.
func (c *client) head() string {
	c.t.Helper()
	got := c.ok("log", "-n", "1", "--json")
	var page struct {
		Commits []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &page); err != nil || len(page.Commits) == 0 {
		c.t.Fatalf("log -n 1 --json: %v\n%s", err, got.stdout)
	}
	return page.Commits[0].SHA
}

// TestE2EOrigoReadsCommitsAndReverts is spec 025's end-to-end criterion: the
// command drives a running node through the whole scenario, with no clone and
// no git process of its own.
func TestE2EOrigoReadsCommitsAndReverts(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	owner, slug := "acme", "app-"+id[:8]
	n.createRepo(id, owner, slug)
	// The history the command reads is pushed with the real git, so what it
	// reads is a real repository and not a fixture of its own making.
	f := gittest.NewFixture(t, "")
	mustGit(t, f.Dir, "push", "-q", n.url(id), "--all")
	mustGit(t, f.Dir, "push", "-q", n.url(id), "--tags")
	s.authz.SetDirectory(true, authorizer.DirectoryEntry{ID: id, Owner: owner, Slug: slug})

	c := newClient(t, n, id, "write")

	// A repository-bound token cannot list, whatever directory the
	// authorizer has: its decision was made at minting, for one repository,
	// so internal/auth refuses the list action on its scope before the
	// authorizer is asked. The line says what to do instead.
	repos := c.run("repos")
	if repos.code != 1 || !strings.Contains(repos.stderr, "action=list") {
		t.Fatalf("repos under a bound token: %d %q", repos.code, repos.stderr)
	}
	if !strings.Contains(repos.stderr, "-repo") || !strings.Contains(repos.stderr, "token from the issuer") {
		t.Fatalf("the refusal does not say what to do instead: %q", repos.stderr)
	}

	// One call orients the caller, and the whole head is on the line because
	// it is what -expect takes.
	info := c.ok("info")
	if !strings.Contains(info.stdout, "name     "+owner+"/"+slug) {
		t.Fatalf("info:\n%s", info.stdout)
	}

	// A file read without a clone.
	list := c.ok("ls", "-r", "-n", "0")
	if len(strings.Fields(list.stdout)) == 0 {
		t.Fatalf("ls -r:\n%s", list.stdout)
	}
	var aFile string
	for _, line := range strings.Split(strings.TrimSpace(list.stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && !strings.HasSuffix(fields[1], "/") {
			aFile = fields[1]
			break
		}
	}
	if aFile == "" {
		t.Fatalf("no file in the listing:\n%s", list.stdout)
	}
	// The pipeline the whole shape is argued on: one command, one grep, no
	// clone. The grep runs here rather than in a shell so a failure names the
	// file it could not find.
	if !strings.Contains(list.stdout, aFile) {
		t.Fatalf("origo ls -r -n 0 | grep %s found nothing", aFile)
	}
	cat := c.ok("cat", aFile)
	if cat.stdout == "" {
		t.Fatalf("cat %s printed nothing; stderr: %s", aFile, cat.stderr)
	}
	if strings.Contains(cat.stdout, "@") && strings.Contains(cat.stdout, "bytes  ") {
		t.Fatal("the header reached stdout, so a redirect would write it into the file")
	}
	if !strings.Contains(cat.stderr, aFile+"@") {
		t.Fatalf("the header is not on stderr: %q", cat.stderr)
	}

	// Twenty commits of history, and the author filter the shell gives.
	log := c.ok("log", "-n", "20")
	if len(strings.Split(strings.TrimSpace(log.stdout), "\n")) == 0 {
		t.Fatalf("log:\n%s", log.stdout)
	}
	before := c.head()
	if len(before) != 40 {
		t.Fatalf("the head is %q", before)
	}
	c.ok("show", before)

	// Two files committed against the head that was read.
	if err := os.WriteFile(filepath.Join(c.dir, "agent.md"), []byte("written by an agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(c.dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, "notes", "second.md"), []byte("and a second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	receipt := c.ok("commit", "-m", "two files from the command", "-expect", before, "agent.md", "notes/second.md")
	if !strings.Contains(receipt.stdout, "committed") {
		t.Fatalf("receipt: %q", receipt.stdout)
	}
	after := c.head()
	if after == before {
		t.Fatal("the branch did not move")
	}

	// The new head is visible, and the commit's author is the variable's.
	info = c.ok("info")
	if !strings.Contains(info.stdout, after) {
		t.Fatalf("info does not carry the new head:\n%s", info.stdout)
	}
	byAuthor := c.ok("log", "-n", "200")
	if !strings.Contains(byAuthor.stdout, "Ada Lovelace") {
		t.Fatalf("origo log -n 200 | grep 'Ada Lovelace' found nothing:\n%s", byAuthor.stdout)
	}

	// Stat first, then one file's patch.
	diff := c.ok("diff", before, after)
	if !strings.Contains(diff.stdout, "agent.md") || strings.Contains(diff.stdout, "@@") {
		t.Fatalf("diff is not stat first:\n%s", diff.stdout)
	}
	patch := c.ok("diff", "-p", "-path", "agent.md", before, after)
	if !strings.Contains(patch.stdout, "written by an agent") {
		t.Fatalf("the narrowed patch:\n%s", patch.stdout)
	}

	// The recovery path is additive: the mistake stays in the history.
	c.ok("revert", "-expect", after, after)
	reverted := c.head()
	if reverted == after {
		t.Fatal("the revert did not move the branch")
	}
	// The revert removed the file, so reading it is ref_not_found rather
	// than an answer. That is the proof the revert landed.
	gone := c.run("cat", "agent.md")
	if gone.code != 1 || !strings.Contains(gone.stderr, "ref_not_found") {
		t.Fatalf("the revert did not remove the file: %d %q\n%s", gone.code, gone.stderr, gone.stdout)
	}

	// A third write carrying the stale head is refused rather than clobbering.
	stale := c.run("commit", "-m", "against a head that moved", "-expect", after, "agent.md")
	if stale.code != 1 {
		t.Fatalf("the stale write exited %d: %s", stale.code, stale.stderr)
	}
	if !strings.Contains(stale.stderr, "non_fast_forward") || !strings.Contains(stale.stderr, "actual="+reverted) {
		t.Fatalf("the refusal did not name the actual head: %q", stale.stderr)
	}
	if c.head() != reverted {
		t.Fatal("the refused write moved the branch")
	}

	// A short -expect is expanded before the write is sent.
	if err := os.WriteFile(filepath.Join(c.dir, "short.md"), []byte("a short id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.ok("commit", "-m", "a short expected head", "-expect", reverted[:7], "short.md")

	// merge and cherry-pick reach their routes on a real node, so every
	// command of the surface is exercised in this run.
	tip := c.head()
	c.ok("commit", "-m", "a branch to merge", "-create", "-branch", "topic", "-from", tip, "short.md")
	topicTip := c.ok("refs", "-prefix", "refs/heads/topic")
	if !strings.Contains(topicTip.stdout, "refs/heads/topic") {
		t.Fatalf("the branch was not created:\n%s", topicTip.stdout)
	}
	c.ok("merge", "-expect", tip, "-strategy", "fast_forward_if_possible", "topic")
	picked := c.head()
	c.ok("cherry-pick", "-expect", picked, "-dry-run", picked)

	// No clone and no git process: the command holds no such code path, and
	// the working directory is the two files the test wrote and nothing else.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			t.Fatal("the command cloned")
		}
	}
	if len(entries) != 3 {
		t.Fatalf("the working directory holds %d entries, want the three the test wrote", len(entries))
	}
}

// TestE2EOrigoReadTokenCannotWrite is the write gate, proved against a running
// node: the credential is what stops a write, and it is enforced by the
// installation rather than by the client.
func TestE2EOrigoReadTokenCannotWrite(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	n.createRepo(id, "acme", "read-"+id[:8])
	f := gittest.NewFixture(t, "")
	mustGit(t, f.Dir, "push", "-q", n.url(id), "--all")

	c := newClient(t, n, id, "read")
	before := c.head()

	// Reading is served.
	c.ok("info")
	c.ok("ls")

	if err := os.WriteFile(filepath.Join(c.dir, "denied.md"), []byte("should not land\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := c.run("commit", "-m", "a write under a read token", "-expect", before, "denied.md")
	if got.code != 1 {
		t.Fatalf("the write exited %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "forbidden") {
		t.Fatalf("the refusal: %q", got.stderr)
	}
	if got.stdout != "" {
		t.Fatalf("a refusal wrote to stdout: %q", got.stdout)
	}
	if c.head() != before {
		t.Fatal("the branch moved under a read token")
	}
}

// TestE2EOrigoDirectoryNeedsAnIssuerToken is the other half of the write
// gate's credential story: `origo repos` is the one command a
// repository-bound token does not have, and a token from the issuer is what
// answers it. Both refusals of a listing say what to do instead.
func TestE2EOrigoDirectoryNeedsAnIssuerToken(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	id := newID(t)
	owner, slug := "acme", "dir-"+id[:8]
	n.createRepo(id, owner, slug)

	// The broader credential: not bound to a repository, so the authorizer
	// decides each call and there is a subject-wide question to ask.
	issuer := &client{
		t: t, bin: origoCommand(t), dir: t.TempDir(),
		env: []string{
			"ORIGO_URL=http://" + n.public,
			"ORIGO_TOKEN=" + s.token,
			"ORIGO_REPO=" + id,
			`ORIGO_AUTHOR=Ada Lovelace <ada@example.com>`,
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
		},
	}

	// An authorizer with no directory says so, and says a repository must be
	// named rather than discovered.
	got := issuer.run("repos")
	if got.code != 1 || !strings.Contains(got.stderr, "directory_unsupported") {
		t.Fatalf("no directory: %d %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "-repo") {
		t.Fatalf("the refusal does not say what to do instead: %q", got.stderr)
	}
	// Naming the repository still works, which is the point of the sentence.
	issuer.ok("info")

	// With a directory, the listing is served.
	s.authz.SetDirectory(true, authorizer.DirectoryEntry{ID: id, Owner: owner, Slug: slug})
	listed := issuer.ok("repos")
	if !strings.Contains(listed.stdout, id) || !strings.Contains(listed.stdout, owner+"/"+slug) {
		t.Fatalf("repos:\n%s", listed.stdout)
	}
	if listed.stderr != "" {
		t.Fatalf("a complete listing wrote to stderr: %q", listed.stderr)
	}

	// A repository-bound token is refused the same call, on its scope.
	bound := newClient(t, n, id, "read")
	refused := bound.run("repos")
	if refused.code != 1 || !strings.Contains(refused.stderr, "action=list") {
		t.Fatalf("repos under a bound token: %d %q", refused.code, refused.stderr)
	}
}

// TestE2EOrigoDocCommandsRun is the document's own test: every sh block of
// docs/cli.md runs against the node, so the page cannot document a command
// that does not work. It mirrors spec 014's TestClusterMigrationDocCommandsRun
// with the one-node harness in place of requireNodes.
func TestE2EOrigoDocCommandsRun(t *testing.T) {
	s := requireStack(t)
	n := startNode(t, s, "", nil)
	// The stack answers, so the document is expected to run: a missing tool
	// is a failure and not a skip, because a skip here would leave the
	// criterion green and unproven.
	for _, bin := range []string{"curl", "uuidgen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("the document needs %s on PATH: %v", bin, err)
		}
	}
	root := repoRoot(t)
	// The document runs origo by name, so the binary this checkout builds
	// goes in front of PATH.
	binDir := filepath.Dir(origoCommand(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(root, "tools", "docs", "run-blocks.sh"), "docs/cli.md")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ORIGO_TEST_URL=http://"+n.public,
		"ORIGO_TEST_ADMIN_TOKEN="+s.token,
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run-blocks.sh docs/cli.md: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "token minted") {
		t.Fatalf("the document did not reach the token:\n%s", out.String())
	}
}
