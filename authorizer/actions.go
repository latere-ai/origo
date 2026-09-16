// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer

import (
	"slices"

	"latere.ai/x/pkg/authz"
)

// KindRepository is the resource kind of every envelope Origo sends. One
// kind spans the whole vocabulary: everything origod asks about is a
// repository, and repo.list names the kind with no id.
const KindRepository = "Repository"

// The four actions of Origo spec 028's table, which are also the value
// of details.action on a 403 forbidden. These four strings are declared
// here and nowhere else in the module: internal/contract reads them from
// here, and so does every consumer outside it.
const (
	// ActionRead is a clone, a fetch, an LFS download, the read API and
	// the archive of spec 009, and the three reads of spec 019.
	ActionRead = "repo.read"
	// ActionWrite is a push, an LFS upload, and the server-side git
	// operations of spec 020.
	ActionWrite = "repo.write"
	// ActionAdmin is creating a repository, changing it, transferring,
	// freezing, deleting, importing, collecting garbage, and minting a
	// repository-bound token.
	ActionAdmin = "repo.admin"
	// ActionList is the directory of spec 026: which repositories may
	// this subject see. It names no repository, so its resource carries
	// the kind and no id, and its answer is a page rather than a
	// decision (see PageActions).
	ActionList = "repo.list"
)

// vocabulary is spec 028's table as data, in the spec's order: every
// action origod asks, each paired with the resource kind it acts on. It
// is the package's one declaration of that table, and Vocabulary,
// Actions, Kind, and Known are four readings of the same value.
var vocabulary = must(authz.NewVocabulary("origo",
	authz.Action{Name: ActionRead, Kind: KindRepository},
	authz.Action{Name: ActionWrite, Kind: KindRepository},
	authz.Action{Name: ActionAdmin, Kind: KindRepository},
	authz.Action{Name: ActionList, Kind: KindRepository},
))

// must is the constructor's error, which is a mistake in the table above
// and in no caller: an entry with no name or no kind, or an action named
// twice. A table that cannot be read as a map from action to kind is a
// programming error, so the package refuses to load rather than letting
// half a table answer a question.
func must(v authz.Vocabulary, err error) authz.Vocabulary {
	if err != nil {
		panic("authorizer: " + err.Error())
	}
	return v
}

// Vocabulary is the whole table as the shared contract's own type, which
// is the form a consumer reads it in: a control plane deciding for Origo
// validates an action and a resource kind against it, a conformance
// suite drives a case per row of it, and origod's own client refuses an
// action outside it before the wire. Import it rather than pairing
// Actions with Kind by hand. The value is fresh on every call, so a
// caller may sort or trim what it gets.
func Vocabulary() authz.Vocabulary {
	return authz.Vocabulary{Core: vocabulary.Core, Actions: slices.Clone(vocabulary.Actions)}
}

// Actions lists every action of the vocabulary, in the table's order.
func Actions() []string {
	out := make([]string, 0, len(vocabulary.Actions))
	for _, a := range vocabulary.Actions {
		out = append(out, a.Name)
	}
	return out
}

// PageActions names the actions whose answer is a page of Origo's own
// shape and not a decision: repo.list, and nothing else. The shared
// vocabulary holds no such flag, because the shape is the core's rather
// than the contract's, so an endpoint declares it by name in
// authz/server's Options.PageActions and a suite in conformance's
// WithPageActions. Pass this rather than the string: an action that
// starts answering a page is a change here and not in each endpoint.
func PageActions() []string { return []string{ActionList} }

// Kind is the resource kind an action acts on: Repository for each of
// the four, and "" for a string outside the vocabulary.
func Kind(action string) string {
	kind, _ := vocabulary.Kind(action)
	return kind
}

// Known reports whether action is one of the vocabulary.
func Known(action string) bool { return vocabulary.Known(action) }
