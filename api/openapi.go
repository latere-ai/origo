// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package openapi holds the document of spec 030: Origo's HTTP surface
// as OpenAPI 3.1, generated from the endpoint, header and code tables of
// the design specs by `make docs` and committed beside this file.
//
// The node embeds it and answers it unchanged, so the bytes a consumer
// vendors from the repository and the bytes an installation serves are
// one file rather than two renderings a test has to keep equal. Nothing
// here parses or rewrites the document: a parser for a constant the
// build already fixed would be work at start-up for no reader, and it
// would put a YAML package on the node's build list, which spec 001's
// seventh invariant and the depcheck rule of .lateregate.yaml keep
// small.
//
// The package sits in api/ rather than internal/ because //go:embed
// reads no parent directory, and the document's own path, api/openapi.yaml,
// is what consumers vendor by tag.
package openapi

import (
	_ "embed"
	"net/http"
	"strconv"
)

// Document is the committed OpenAPI document, as it is on disk.
//
//go:embed openapi.yaml
var Document []byte

// ContentType is the media type of an OpenAPI document written as YAML,
// RFC 9512.
const ContentType = "application/yaml"

// Handler answers the document. It takes no token: a document that says
// how a caller authenticates is not one a caller can be asked to
// authenticate for, which is why the node mounts it in front of the
// verifier beside GET /version and the key set.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(Document)))
		_, _ = w.Write(Document)
	})
}
