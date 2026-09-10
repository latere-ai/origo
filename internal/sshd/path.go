// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"errors"
	"strings"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/wal"
)

// MaxPathBytes is the longest repository argument accepted, before any
// of it is parsed.
const MaxPathBytes = 4 << 10

// errPath is every refusal of the argument: a client learns that the
// request was malformed and nothing about what exists.
var errPath = errors.New("the repository argument is not a path this server serves")

// ParsePath maps the argument of git-upload-pack or git-receive-pack to
// the repository it names, in the two forms spec 003 defines.
//
// Both client URL shapes produce different strings for one repository:
// scp syntax, git@host:owner/slug.git, sends `owner/slug.git`, while
// ssh://git@host/owner/slug.git sends `/owner/slug.git`. So the leading
// slash and the .git suffix are both optional and neither changes what
// is named.
//
//	/owner/slug.git   owner/slug.git   /owner/slug   owner/slug   the name form
//	/r/<id>.git       r/<id>.git       /r/<id>       r/<id>       the id form
//
// ~owner/slug.git is refused: the tilde form is a home-directory
// convention and Origo has no filesystem to resolve it against.
// Everything else is refused too. Owner and slug pass spec 003's
// grammar here, so a path is never a string a subprocess sees.
func ParsePath(arg string) (auth.RepoRef, error) {
	if arg == "" || len(arg) > MaxPathBytes {
		return auth.RepoRef{}, errPath
	}
	if strings.ContainsAny(arg, "\x00\n\r") {
		return auth.RepoRef{}, errPath
	}
	p := strings.TrimSuffix(strings.TrimPrefix(arg, "/"), ".git")
	owner, slug, ok := strings.Cut(p, "/")
	if !ok || owner == "" || slug == "" || strings.Contains(slug, "/") {
		return auth.RepoRef{}, errPath
	}
	if owner == "r" {
		if !wal.ValidID(slug) {
			return auth.RepoRef{}, errPath
		}
		return auth.RepoRef{ID: slug}, nil
	}
	if !validName(owner) || !validName(slug) {
		return auth.RepoRef{}, errPath
	}
	return auth.RepoRef{Owner: owner, Slug: slug}, nil
}

// ReservedOwners are the path segments the public surface uses itself
// (spec 003): the id form and the JSON API share the name form's path
// space, so neither is a name a repository can take.
var ReservedOwners = map[string]bool{"r": true, "v1": true}

// validName is spec 003's grammar for an owner or a slug, which is
// wal.ValidLabel, the same function the create endpoint applies, with
// the reserved segments removed. Nothing is reimplemented here: a path
// this package accepts is a name the log would accept.
func validName(s string) bool { return wal.ValidLabel(s) && !ReservedOwners[s] }

// ParseCommand splits an exec request into the service and its one
// argument. The surface is a maintained list of two entries and
// everything else is refused, so a shell, git-upload-archive, and
// git-lfs-authenticate all end in the same place.
//
// The quoting is the one convention git emits: the argument in single
// quotes with an embedded quote written '\”. It is parsed here and
// never by a shell, because there is no shell on this path.
func ParseCommand(line string) (service, arg string, err error) {
	name, rest, ok := strings.Cut(line, " ")
	if !ok {
		return "", "", errCommand
	}
	if name != ServiceUploadPack && name != ServiceReceivePack {
		return "", "", errCommand
	}
	arg, err = unquote(rest)
	if err != nil {
		return "", "", err
	}
	if arg == "" {
		return "", "", errCommand
	}
	return name, arg, nil
}

// The two commands an SSH connection to Origo may run.
const (
	ServiceUploadPack  = "git-upload-pack"
	ServiceReceivePack = "git-receive-pack"
)

var errCommand = errors.New("only git-upload-pack and git-receive-pack run on this connection")

// unquote reads git's shell quoting without a shell: a single-quoted
// string with '\” for an embedded quote, or a bare word with no shell
// metacharacter in it.
func unquote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errCommand
	}
	if s[0] != '\'' {
		if strings.ContainsAny(s, " \t'\"\\$`;&|<>()\n\r\x00") {
			return "", errCommand
		}
		return s, nil
	}
	var b strings.Builder
	i := 1
	for {
		j := strings.IndexByte(s[i:], '\'')
		if j < 0 {
			return "", errCommand
		}
		b.WriteString(s[i : i+j])
		i += j + 1
		if i == len(s) {
			return b.String(), nil
		}
		// git writes an embedded quote as '\'': close, escape, reopen.
		if strings.HasPrefix(s[i:], `\''`) {
			b.WriteByte('\'')
			i += 3
			continue
		}
		return "", errCommand
	}
}
