// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package origocli is the `origo` command: the flags, the byte defaults, the
// line writers and the exit codes of spec 025.
//
// It reaches the network only through [github.com/latere-ai/origo/internal/origoclient],
// which speaks contract 1 and formats nothing. Everything a reader sees is
// decided here, under three rules.
//
// Data goes to stdout and every header, truncation and stale line goes to
// stderr, through the one [out.notice] chokepoint. That is what makes
// `origo ls -r -n 0 | grep -i handler` filter only payload and
// `origo cat x.go > x.go` write only the file.
//
// The default answer is the smallest one that answers the question, and the
// flag that gives more is named on stderr where the answer was cut.
//
// The truncation an answer reports is the truncation that happened: where
// Origo cut a body itself the notice says so and never presents a partial
// list as a complete one.
package origocli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/latere-ai/origo/internal/origoclient"
	versionpkg "github.com/latere-ai/origo/internal/version"
)

// The exit codes. They are the shell's half of the contract: 0 served, 1 the
// installation or the network refused, 2 the caller or the environment is
// wrong and no request was made.
const (
	CodeOK       = 0
	CodeRefused  = 1
	CodeMisusage = 2
)

// Getenv is the environment, replaceable in a test.
type Getenv func(string) string

// out is the two writers, and the one place a line chooses between them.
type out struct {
	data   io.Writer
	notes  io.Writer
	quiet  bool
	staled bool
}

// line writes one datum. Everything a pipeline reads goes through here.
func (o *out) line(format string, a ...any) {
	_, _ = fmt.Fprintf(o.data, format+"\n", a...)
}

// write writes bytes verbatim, for a file's own content.
func (o *out) write(b []byte) { _, _ = o.data.Write(b) }

// notice writes one line about the answer rather than of it: a header, a
// truncation, a staleness. It can only ever reach stderr, which is what makes
// the criterion structural instead of a promise repeated per command.
func (o *out) notice(format string, a ...any) {
	if o.quiet {
		return
	}
	_, _ = fmt.Fprintf(o.notes, format+"\n", a...)
}

// stale reports one Origo-Stale header, once per run: a caller that reads a
// head, then writes, gets non_fast_forward and should know why. Repeating it
// per page would spend a reader's attention on one fact many times.
func (o *out) stale(m origoclient.Meta) {
	if m.Stale == "" || o.staled {
		return
	}
	o.staled = true
	o.notice("%s", staleLine(m.Stale))
}

// config is the four variables of spec 025, already checked.
type config struct {
	url    string
	token  string
	repo   string
	author string
}

// Run is the whole command. It returns the process exit code, so a test drives
// it without a subprocess.
func Run(ctx context.Context, args []string, getenv Getenv, stdout, stderr io.Writer) int {
	o := &out{data: stdout, notes: stderr}
	name, rest := subcommand(args)
	switch name {
	case "", "help", "-h", "-help", "--help":
		usage(stdout)
		if name == "" {
			return CodeMisusage
		}
		return CodeOK
	case "version":
		_, _ = fmt.Fprintln(stdout, versionpkg.String())
		return CodeOK
	}
	cmd, ok := commands[name]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "origo: unknown command %q; run origo help\n", name)
		return CodeMisusage
	}
	cfg, err := load(getenv, cmd.writes)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "origo:", err)
		return CodeMisusage
	}
	c := origoclient.New(origoclient.Config{URL: cfg.url, Token: cfg.token})
	if err := cmd.run(ctx, &session{cfg: cfg, client: c, out: o}, rest); err != nil {
		return report(o, err)
	}
	return CodeOK
}

// subcommand is spec 002's rule, which this command borrows so the two
// binaries parse a command line the same way: the first argument that does not
// start with a dash names the command, and the arguments around it are its
// own.
func subcommand(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			return a, append(rest, args[i+1:]...)
		}
	}
	if len(args) > 0 {
		return args[0], nil // a lone -h or -version
	}
	return "", args
}

// usageError is a caller's mistake: exit 2, and no request was made.
type usageError struct{ text string }

func (u *usageError) Error() string { return u.text }

func misuse(format string, a ...any) error {
	return &usageError{text: fmt.Sprintf(format, a...)}
}

// report turns whatever ended a command into one line on stderr and the exit
// code that goes with it. Spec 003's envelope becomes `<code>: <message>` with
// that row's details and nothing else; the response body never reaches a
// reader.
func report(o *out, err error) int {
	if use, ok := errors.AsType[*usageError](err); ok {
		o.notice("origo: %s", use.text)
		return CodeMisusage
	}
	if ref, ok := origoclient.AsRefusal(err); ok {
		o.notice("%s", refusalLine(ref))
		return CodeRefused
	}
	o.notice("origo: %s", err.Error())
	return CodeRefused
}

// load reads and checks the four variables. Every refusal here happens before
// a request, so a mistyped value costs no round trip.
func load(getenv Getenv, writes bool) (config, error) {
	cfg := config{
		url:    strings.TrimRight(strings.TrimSpace(getenv("ORIGO_URL")), "/"),
		token:  strings.TrimSpace(getenv("ORIGO_TOKEN")),
		repo:   strings.TrimSpace(getenv("ORIGO_REPO")),
		author: strings.TrimSpace(getenv("ORIGO_AUTHOR")),
	}
	if cfg.url == "" {
		return cfg, errors.New("set ORIGO_URL to the installation, https://git.example.com")
	}
	u, err := url.Parse(cfg.url)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return cfg, errors.New("ORIGO_URL is an absolute http or https URL")
	}
	if u.Scheme != "https" && !loopback(u.Hostname()) {
		return cfg, errors.New("ORIGO_URL is https, except at a loopback address")
	}
	if cfg.token == "" {
		return cfg, errors.New("set ORIGO_TOKEN to a bearer; mint a repository-bound one with POST /v1/repos/{id}/tokens")
	}
	if strings.HasPrefix(cfg.token, "-----BEGIN") {
		// ORIGO_TOKEN_KEY is the node's signing key and sits one word away.
		// Sending it as a bearer would put a private key in a header.
		return cfg, errors.New("ORIGO_TOKEN holds a PEM block; that is ORIGO_TOKEN_KEY, the node's signing key, not a bearer")
	}
	if writes {
		if cfg.author == "" {
			return cfg, errors.New(`set ORIGO_AUTHOR to "Name <email>"; a write records who made it`)
		}
		if _, err := parseAuthor(cfg.author); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func loopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

// parseAuthor splits `Name <email>` into the two fields spec 020's author
// object carries. The node refuses an address with no @, so this refuses it
// first and names the variable rather than the request.
func parseAuthor(s string) (origoclient.Author, error) {
	open := strings.LastIndex(s, "<")
	closeAt := strings.LastIndex(s, ">")
	if open < 0 || closeAt < open {
		return origoclient.Author{}, errors.New(`ORIGO_AUTHOR is "Name <email>"`)
	}
	a := origoclient.Author{
		Name:  strings.TrimSpace(s[:open]),
		Email: strings.TrimSpace(s[open+1 : closeAt]),
	}
	if a.Name == "" || !strings.Contains(a.Email, "@") {
		return origoclient.Author{}, errors.New(`ORIGO_AUTHOR is "Name <email>", and the address holds an @`)
	}
	return a, nil
}

// session is one command's world: the checked configuration, the client, and
// the two writers.
type session struct {
	cfg      config
	client   *origoclient.Client
	out      *out
	resolved string
}

// command is one entry of the surface. The argument line lives in [usages]
// rather than here: a run function names its own usage, so holding both in one
// map would be an initialization cycle.
type command struct {
	blurb  string
	writes bool
	run    func(context.Context, *session, []string) error
}

// flags builds the flag set every command shares the shape of. Parsing writes
// to stderr, because a usage message is about the answer and not of it.
func (s *session) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("origo "+name, flag.ContinueOnError)
	fs.SetOutput(s.out.notes)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(s.out.notes, "usage: origo %s %s\n", name, usages[name])
		fs.PrintDefaults()
	}
	return fs
}

// repoFlag adds -repo, which takes an id or <owner>/<slug>.
func (s *session) repoFlag(fs *flag.FlagSet) *string {
	return fs.String("repo", s.cfg.repo, "the repository: an id, or owner/slug")
}

// repoID resolves what -repo or ORIGO_REPO named, once per process. A name
// goes through the name mode of GET /v1/repos; an id is used as it stands.
func (s *session) repoID(ctx context.Context, named string) (string, error) {
	if named == "" {
		return "", misuse("name a repository with -repo or ORIGO_REPO")
	}
	if !strings.Contains(named, "/") {
		return named, nil
	}
	if s.resolved != "" {
		return s.resolved, nil
	}
	owner, slug, _ := strings.Cut(named, "/")
	if owner == "" || slug == "" || strings.Contains(slug, "/") {
		return "", misuse("a repository name is owner/slug")
	}
	repo, err := s.client.Resolve(ctx, owner, slug)
	if err != nil {
		return "", err
	}
	s.resolved = repo.ID
	return repo.ID, nil
}

// commands is the surface. Twelve, and the names are git's where git has one,
// because a name a caller already knows costs no explanation.
var commands = map[string]command{
	"repos":       {blurb: "the repositories this credential may see", run: runRepos},
	"info":        {blurb: "one repository in one call", run: runInfo},
	"refs":        {blurb: "branches or tags with their object ids", run: runRefs},
	"ls":          {blurb: "the entries of a tree", run: runLs},
	"cat":         {blurb: "a file's text, windowed", run: runCat},
	"log":         {blurb: "history, one line per commit", run: runLog},
	"show":        {blurb: "one commit, stat first", run: runShow},
	"diff":        {blurb: "a comparison, stat first", run: runDiff},
	"commit":      {blurb: "a commit from named local files", writes: true, run: runCommit},
	"merge":       {blurb: "merge a source into a branch", writes: true, run: runMerge},
	"cherry-pick": {blurb: "replay commits onto a branch", writes: true, run: runCherryPick},
	"revert":      {blurb: "replay commits backwards", writes: true, run: runRevert},
}

// usages is the argument line of each command, held apart from [commands] so
// a run function can name its own without an initialization cycle.
var usages = map[string]string{
	"repos":       "[-n 200] [--json]",
	"info":        "[--json]",
	"refs":        "[-tags|-all] [-prefix p] [-n 100] [--json]",
	"ls":          "[-r] [-n 200] [-ref r] [path] [--json]",
	"cat":         "[-n 800] [-offset 0] [-max-bytes 32768] [-ref r] <path>",
	"log":         "[-n 20] [-ref r] [-path p] [-since t] [-until t] [--json]",
	"show":        "[-p] [-path p] [-max-bytes 32768] <commit>",
	"diff":        "[-p] [-path p] [-max-bytes 32768] <base> <head>",
	"commit":      "-m <message> -expect <sha> [-branch b] [-delete p] [-file r=l] [-create] [-from ref] [-dry-run] <path>...",
	"merge":       "-expect <sha> [-branch b] [-strategy s] [-m msg] [-dry-run] <source>",
	"cherry-pick": "-expect <sha> [-branch b] [-mainline n] [-dry-run] <sha>...",
	"revert":      "-expect <sha> [-branch b] [-mainline n] [-dry-run] <sha>...",
}

// order is the surface in the order a reader meets it, which is the order the
// document and the skill use: read before write, orient before descend.
var order = []string{"repos", "info", "refs", "ls", "cat", "log", "show", "diff", "commit", "merge", "cherry-pick", "revert"}

// usage is the help page. It is read on demand and never resident, which is
// the whole advantage of the shape, so it can afford to be complete.
func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `origo reads and changes a repository on an Origo installation without cloning it.

usage: origo <command> [flags] [arguments]

environment:
  ORIGO_URL      the installation, https://git.example.com
  ORIGO_TOKEN    a bearer; mint a repository-bound one with POST /v1/repos/{id}/tokens
  ORIGO_REPO     the repository every command works on: an id, or owner/slug
  ORIGO_AUTHOR   "Name <email>", the author of every commit a write command makes

commands:
`)
	for _, name := range order {
		_, _ = fmt.Fprintf(w, "  %-12s %s\n", name, commands[name].blurb)
	}
	_, _ = fmt.Fprint(w, `  version      the build identity
  help         this page

Every read command takes --json and prints the contract's own JSON. Data goes
to standard output and every header, truncation and stale line to standard
error, so a pipeline filters only payload:

  origo ls -r -n 0 | grep -i handler
  origo log -n 200 | grep '<name>'
`)
}
