// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package authorizer is the vocabulary an authorization endpoint for
// Origo is written against: the four actions origod asks, and the one
// resource kind it names them on. Import it to write the endpoint
// ORIGO_AUTHORIZER_URL points at, in Go, instead of keeping a copy of
// the strings.
//
// The envelope on the wire is latere.ai/x/pkg/authz's, which Origo spec
// 028 names contract 2: origod POSTs the caller's subject, its verified
// claims, an action, and a resource of kind Repository, and reads back
// an allow or a deny. This package is the Origo half of that contract.
// Nothing here dials; it declares values, and the client is the
// caller's.
//
// Vocabulary is the table in one value, of the shared contract's own
// type. It is what an endpoint validates a request against and what
// latere.ai/x/pkg/authz/conformance drives its cases from, and Actions,
// Kind, and Known are three readings of that same value, so a consumer
// holds one declaration and never a copy of it:
//
//	http.Handle("POST /authorize", server.New(server.Options{
//		Bearer:      os.Getenv("AUTHORIZER_TOKEN"),
//		Vocabulary:  authorizer.Vocabulary(),
//		PageActions: authorizer.PageActions(),
//		Decider:     policy{},
//		Lister:      directory{},
//	}))
//
// repo.list is the one action whose answer is not a decision. It names
// no repository and asks which repositories a subject may see, and the
// endpoint answers a page of Origo's own shape: the repositories and a
// next cursor, or a deny with a reason, or an installation that has no
// directory at all (Origo spec 026). The shared vocabulary carries no
// flag for that shape, so PageActions names the action instead, for
// authz/server's Options.PageActions and for the conformance suite's
// WithPageActions. The other three actions answer an allow or a deny
// with the optional limits object.
//
// The promise, as for every package at this module's root: additive
// within a module major, and the same on every build. An action string
// never changes and never disappears, and a new action is a new row in
// Origo spec 028 first and a constant here second, so an endpoint that
// decides by the constants keeps compiling and one that decides by a
// default keeps deciding.
package authorizer
