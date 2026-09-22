// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/goccy/go-yaml"

	"latere.ai/x/origo/tools/specindex/specs"
)

// The document's own identity. The version is not written here: it is the
// contract version spec 003 states, read from the deck like everything
// else on the page.
const (
	openapiVersion = "3.1.0"
	docTitle       = "Origo"
	docDescription = "Git hosting as an infrastructure component: repositories addressed by a caller-chosen id, git over HTTP and SSH, and a JSON API for the repository lifecycle, reads, LFS, administration and server-side git operations. Every operation below is one row of a design spec; see the tags for where a route lives."
)

// header is the comment the committed file opens with. It is part of the
// bytes a node serves, so it says what the document is to a person who
// fetched it from an installation as well as to one reading the file.
const header = `# Origo's HTTP surface as an OpenAPI 3.1 document, generated from the
# endpoint, header and code tables of the design specs by ` + "`make docs`" + `.
# Do not edit: a test fails when this file is not what a fresh run
# produces, and the specs are where a route's behaviour is stated.
#
# It names no server, because a document in a repository describes no one
# installation, and no release, because what a client pins is the contract
# version in info.version. A consumer vendoring this file records the tag
# it took it from.
`

// bearer is the name the one security scheme is referenced by.
const bearer = "bearer"

// tagsBySpec is where a reader finds each spec's routes. A group is a
// decision about presentation and not about meaning, so it is written
// once, here, and a spec that defines a route outside /{repo}/ without a
// row is an error rather than a fall-through into an unnamed group.
var tagsBySpec = map[string]string{
	"002": "service",
	"003": "repositories",
	"007": "tokens",
	"009": "read",
	"014": "administration",
	"019": "administration",
	"020": "operations",
	"022": "service",
	"026": "repositories",
	"030": "service",
}

// summaries names each action for navigation, independently of the endpoint
// table's full behavior prose. Every route must have an explicit label: adding
// a route without deciding its name fails generation instead of truncating prose.
var summaries = map[string]string{
	"GET /":                      "Get landing page",
	"GET /.well-known/jwks.json": "Get signing keys",
	"GET /favicon.ico":           "Get favicon",
	"GET /livez":                 "Check liveness",
	"GET /metrics":               "Get metrics",
	"GET /openapi.yaml":          "Get OpenAPI document",
	"GET /readyz":                "Check readiness",
	"GET /version":               "Get version",
	"GET /v1/repos":              "List repositories",
	"POST /v1/repos":             "Create repository",
	"GET /v1/repos/{id}":         "Get repository",
	"PATCH /v1/repos/{id}":       "Update repository",
	"DELETE /v1/repos/{id}":      "Delete repository",
	"GET /v1/repos/{id}/archive/{sha}.tar.gz":    "Download archive",
	"GET /v1/repos/{id}/blob/{sha}":              "Get blob",
	"POST /v1/repos/{id}/cherry-pick":            "Cherry-pick commits",
	"GET /v1/repos/{id}/commits":                 "List commits",
	"POST /v1/repos/{id}/commits":                "Create commit",
	"GET /v1/repos/{id}/commits/{sha}":           "Get commit",
	"GET /v1/repos/{id}/compare/{base}...{head}": "Compare revisions",
	"GET /v1/repos/{id}/export.bundle":           "Export repository",
	"POST /v1/repos/{id}/freeze":                 "Freeze repository",
	"POST /v1/repos/{id}/gc":                     "Compact repository",
	"GET /v1/repos/{id}/import":                  "Get import status",
	"POST /v1/repos/{id}/import":                 "Import repository",
	"POST /v1/repos/{id}/merge":                  "Merge revisions",
	"GET /v1/repos/{id}/refs":                    "List references",
	"POST /v1/repos/{id}/revert":                 "Revert commits",
	"GET /v1/repos/{id}/stats":                   "Get repository statistics",
	"POST /v1/repos/{id}/tokens":                 "Create repository token",
	"POST /v1/repos/{id}/transfer":               "Transfer repository",
	"GET /v1/repos/{id}/tree/{sha}":              "List files",
	"POST /v1/repos/{id}/undelete":               "Restore repository",
	"POST /v1/repos/{id}/unfreeze":               "Unfreeze repository",
	"POST /v1/repos/{id}/verify":                 "Verify repository",
	"POST /{repo}/git-receive-pack":              "Push changes",
	"POST /{repo}/git-upload-pack":               "Fetch repository",
	"POST /{repo}/info/lfs/locks":                "Request LFS lock",
	"POST /{repo}/info/lfs/objects/batch":        "Get LFS transfer actions",
	"POST /{repo}/info/lfs/verify":               "Verify LFS upload",
	"GET /{repo}/info/refs":                      "Advertise references",
}

// tagDescriptions are the one line each group carries in the document.
var tagDescriptions = map[string]string{
	"administration": "Rename, transfer, freeze, import, export, statistics and garbage collection.",
	"operations":     "Commits, merges, cherry-picks and reverts made by the node, without a clone.",
	"read":           "References, commits, diffs, trees, blobs and archives, served by git from the node's copy.",
	"repositories":   "The repository lifecycle and the directory a caller is allowed to see.",
	"service":        "What an installation answers about itself. Not part of a caller's integration.",
	"tokens":         "Repository-bound tokens and the key set they are verified against.",
	"transport":      "git over HTTP and the LFS transfer API, at the host the clone URLs name.",
}

// scopesBySpec marks the rows a caller writing against the API never
// sends: the probes of spec 002 and the pages of spec 022. A consumer
// hides them by this mark or by their tag.
var scopesBySpec = map[string]string{"002": "operator", "022": "operator"}

// unauthenticated is every row the deck serves without a bearer token,
// with the sentence that says so: spec 002's probes ("unauthenticated
// like GET /readyz and GET /version"), spec 007's key set ("no token
// required"), spec 022's page and favicon ("no token"), and this
// document's own route. TestTheDocumentsUnauthenticatedRoutesAreTheNodes
// in cmd/origod holds this list to the node that serves them.
var unauthenticated = map[string]bool{
	"GET /livez":                 true,
	"GET /readyz":                true,
	"GET /version":               true,
	"GET /metrics":               true,
	"GET /.well-known/jwks.json": true,
	"GET /":                      true,
	"GET /favicon.ico":           true,
	"GET /openapi.yaml":          true,
}

// everyRefusal is what a route behind the verifier answers before any
// handler of it runs: no token, a deny from the authorizer, and the rate
// of spec 012. They are on every secured operation because the
// middleware is, not because a row states them.
var everyRefusal = []string{"unauthenticated", "forbidden", "rate_limited"}

// Document is the OpenAPI description. The fields are in the order a
// reader expects them and every map the standard library renders is
// sorted by key, so two runs over one deck are one file.
type Document struct {
	OpenAPI    string              `json:"openapi"`
	Info       Info                `json:"info"`
	Tags       []Tag               `json:"tags,omitempty"`
	Paths      map[string]PathItem `json:"paths"`
	Components Components          `json:"components"`
}

// Info is the document's own identity.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// Tag is one group of operations.
type Tag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PathItem is the operations of one path, keyed by the lower-case method.
type PathItem map[string]Operation

// Operation is one endpoint row.
type Operation struct {
	OperationID string              `json:"operationId"`
	Summary     string              `json:"summary,omitempty"`
	Description string              `json:"description,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	Scope       string              `json:"x-scope,omitempty"`
	Parameters  []Parameter         `json:"parameters,omitempty"`
	Security    []map[string][]any  `json:"security"`
	Responses   map[string]Response `json:"responses"`
}

// Parameter is one wildcard of a path or one query field the row states.
type Parameter struct {
	Name        string `json:"name"`
	In          string `json:"in"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	Schema      Schema `json:"schema"`
}

// Response is one answer, or a reference to one the components declare.
type Response struct {
	Ref         string               `json:"$ref,omitempty"`
	Description string               `json:"description,omitempty"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// MediaType is one content type of a response.
type MediaType struct {
	Schema Schema `json:"schema"`
}

// Schema is a JSON Schema, as much of one as this document needs.
type Schema struct {
	Ref         string            `json:"$ref,omitempty"`
	Type        string            `json:"type,omitempty"`
	Description string            `json:"description,omitempty"`
	Properties  map[string]Schema `json:"properties,omitempty"`
	Required    []string          `json:"required,omitempty"`
}

// Components are the pieces the operations reference: the bearer scheme,
// the error envelope, and one response per code of the deck.
type Components struct {
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes"`
	Schemas         map[string]Schema         `json:"schemas"`
	Responses       map[string]Response       `json:"responses"`
}

// SecurityScheme is the one credential the surface takes.
type SecurityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
	Description  string `json:"description,omitempty"`
}

// code is one row of a Code table: the statuses its Status column lists,
// in that column's order, and its one user sentence.
type code struct {
	statuses []int
	sentence string
}

var (
	wildcardRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	queryRe    = regexp.MustCompile(`[?&]([a-z][a-z0-9_]*)=`)
	statusRe   = regexp.MustCompile(`\b([1-5][0-9]{2})\b`)
	leadingRe  = regexp.MustCompile(`^([1-5][0-9]{2})(\D|$)`)
	backtickRe = regexp.MustCompile("`([^`]+)`")
)

// Build renders the document from the deck.
func Build(idx *specs.Index) (*Document, error) {
	version, err := contract(idx)
	if err != nil {
		return nil, err
	}
	codes, err := codeTable(idx)
	if err != nil {
		return nil, err
	}
	d := &Document{
		OpenAPI: openapiVersion,
		Info:    Info{Title: docTitle, Version: version, Description: docDescription},
		Paths:   map[string]PathItem{},
		Components: Components{
			SecuritySchemes: map[string]SecurityScheme{bearer: {
				Type: "http", Scheme: "bearer", BearerFormat: "JWT",
				Description: "A token from an issuer this installation lists, carrying an audience it verifies. Every route but the ones marked with no security takes one, the git routes included.",
			}},
			Schemas:   schemas(),
			Responses: responses(codes),
		},
	}
	used := map[string]bool{}
	for _, n := range idx.Names {
		if n.Kind != specs.KindEndpoint || len(n.Row) < 2 {
			continue
		}
		method, path, ok := strings.Cut(n.Name, " ")
		if !ok {
			return nil, fmt.Errorf("endpoint %q is not a method and a path", n.Name)
		}
		op, err := operation(n, method, path, codes)
		if err != nil {
			return nil, err
		}
		used[op.Tags[0]] = true
		item, ok := d.Paths[path]
		if !ok {
			item = PathItem{}
			d.Paths[path] = item
		}
		if _, taken := item[strings.ToLower(method)]; taken {
			return nil, fmt.Errorf("%s is defined twice", n.Name)
		}
		item[strings.ToLower(method)] = op
	}
	if len(d.Paths) == 0 {
		return nil, errors.New("no spec defines an endpoint")
	}
	for _, name := range slices.Sorted(maps.Keys(used)) {
		d.Tags = append(d.Tags, Tag{Name: name, Description: tagDescriptions[name]})
	}
	return d, nil
}

// operation renders one endpoint row.
func operation(n specs.Name, method, path string, codes map[string]code) (Operation, error) {
	desc := describe(n)
	tag, err := tagOf(path, n.Owner)
	if err != nil {
		return Operation{}, err
	}
	summary, ok := summaries[n.Name]
	if !ok {
		return Operation{}, fmt.Errorf("endpoint %q has no action summary; add it to summaries", n.Name)
	}
	op := Operation{
		OperationID: operationID(method, path),
		Summary:     summary,
		Description: prose(n, desc),
		Tags:        []string{tag},
		Scope:       scopesBySpec[n.Owner],
		Parameters:  parameters(path, desc),
		Security:    []map[string][]any{},
		Responses:   map[string]Response{},
	}
	secured := !unauthenticated[n.Name]
	if secured {
		op.Security = []map[string][]any{{bearer: {}}}
	}
	for _, status := range success(desc) {
		op.Responses[strconv.Itoa(status)] = Response{Description: http.StatusText(status)}
	}
	named := refusals(desc, codes)
	if secured {
		for _, c := range everyRefusal {
			if _, ok := codes[c]; ok && !slices.Contains(named, c) {
				named = append(named, c)
			}
		}
	}
	byStatus := map[int][]string{}
	for _, c := range named {
		for _, s := range codes[c].statuses {
			byStatus[s] = append(byStatus[s], c)
		}
	}
	for status, sharing := range byStatus {
		sort.Strings(sharing)
		op.Responses[strconv.Itoa(status)] = answer(sharing, codes)
	}
	return op, nil
}

// answer renders one refused status. A status one code of the row
// answers is a reference to that code's declared response; a status
// several share carries each code with its sentence, since OpenAPI keys
// an operation's answers by status and the code is what a caller
// branches on.
func answer(sharing []string, codes map[string]code) Response {
	if len(sharing) == 1 {
		return Response{Ref: "#/components/responses/" + sharing[0]}
	}
	var parts []string
	for _, c := range sharing {
		parts = append(parts, c+": "+codes[c].sentence)
	}
	return Response{
		Description: strings.Join(parts, " "),
		Content:     map[string]MediaType{"application/json": {Schema: Schema{Ref: "#/components/schemas/Error"}}},
	}
}

// describe is the row's cells after the path, padded to the header so a
// short row keeps its columns.
func describe(n specs.Name) []string {
	cells := slices.Clone(n.Row)
	for len(cells) < len(n.Header) {
		cells = append(cells, "")
	}
	if len(cells) < 3 {
		return nil
	}
	return cells[2:]
}

// prose is the operation's description: every column after the path, in
// the table's order, under its own header where the table has more than
// one, so the document carries the row and not a summary of it.
func prose(n specs.Name, desc []string) string {
	var parts []string
	for i, cell := range desc {
		if strings.TrimSpace(cell) == "" {
			continue
		}
		if len(desc) > 1 && i+2 < len(n.Header) {
			parts = append(parts, "**"+n.Header[i+2]+":** "+cell)
			continue
		}
		parts = append(parts, cell)
	}
	return strings.Join(parts, "\n\n")
}

// answerColumn is the column of a row that states what the caller gets:
// the last one after the path, because a table that splits the request
// from the response puts the response last, and a table with one column
// states both in it.
func answerColumn(desc []string) string {
	for i := len(desc) - 1; i >= 0; i-- {
		if strings.TrimSpace(desc[i]) != "" {
			return desc[i]
		}
	}
	return ""
}

// tagOf is the group one row's operation is rendered in: the path
// decides it for the transport, the owning spec for everything else.
func tagOf(path, owner string) (string, error) {
	if strings.HasPrefix(path, "/{repo}/") {
		return "transport", nil
	}
	tag, ok := tagsBySpec[owner]
	if !ok {
		return "", fmt.Errorf("spec %s defines an endpoint and names no group; add it to tagsBySpec", owner)
	}
	return tag, nil
}

// parameters are the wildcards of the path, required, and the query
// fields the row states, optional because each of them has a default the
// same row states.
func parameters(path string, desc []string) []Parameter {
	var out []Parameter
	seen := map[string]bool{}
	for _, m := range wildcardRe.FindAllStringSubmatch(path, -1) {
		if seen["path\x00"+m[1]] {
			continue
		}
		seen["path\x00"+m[1]] = true
		p := Parameter{Name: m[1], In: "path", Required: true, Schema: Schema{Type: "string"}}
		if m[1] == "repo" {
			p.Description = repoAddress
		}
		out = append(out, p)
	}
	for _, cell := range desc {
		for _, m := range queryRe.FindAllStringSubmatch(cell, -1) {
			if seen["query\x00"+m[1]] {
				continue
			}
			seen["query\x00"+m[1]] = true
			out = append(out, Parameter{Name: m[1], In: "query", Schema: Schema{Type: "string"}})
		}
	}
	return out
}

// success is the statuses a row states its answers with: every 2xx it
// names, in the order it names them, or 200 where it names none. An
// answer column that opens with a status outside 2xx is a row whose
// route only refuses, and it gets no success answer at all
// (POST /{repo}/info/lfs/locks).
func success(desc []string) []int {
	if m := leadingRe.FindStringSubmatch(strings.TrimSpace(answerColumn(desc))); m != nil {
		status, _ := strconv.Atoi(m[1])
		if status < 200 || status > 299 {
			return nil
		}
	}
	var out []int
	for _, cell := range desc {
		for _, m := range statusRe.FindAllStringSubmatch(cell, -1) {
			status, _ := strconv.Atoi(m[1])
			if status >= 200 && status < 300 && !slices.Contains(out, status) {
				out = append(out, status)
			}
		}
	}
	if len(out) == 0 {
		return []int{http.StatusOK}
	}
	return out
}

// refusals are the codes the row names, in the order it names them.
func refusals(desc []string, codes map[string]code) []string {
	var out []string
	seen := map[string]bool{}
	for _, cell := range desc {
		for _, m := range backtickRe.FindAllStringSubmatch(cell, -1) {
			if _, ok := codes[m[1]]; !ok || seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// codeTable reads the deck's Code tables: every status the Status column
// lists, in its order, and the one user sentence of the Message column.
func codeTable(idx *specs.Index) (map[string]code, error) {
	out := map[string]code{}
	for _, n := range idx.Names {
		if n.Kind != specs.KindCode {
			continue
		}
		status, ok := column(n, "status")
		if !ok {
			return nil, fmt.Errorf("the table defining %q has no Status column", n.Name)
		}
		message, ok := column(n, "message")
		if !ok {
			return nil, fmt.Errorf("the table defining %q has no Message column", n.Name)
		}
		var statuses []int
		for _, m := range statusRe.FindAllStringSubmatch(status, -1) {
			s, _ := strconv.Atoi(m[1])
			statuses = append(statuses, s)
		}
		if len(statuses) == 0 {
			return nil, fmt.Errorf("code %q states no status", n.Name)
		}
		out[n.Name] = code{statuses: statuses, sentence: strings.TrimSpace(message)}
	}
	if len(out) == 0 {
		return nil, errors.New("no spec defines an error code")
	}
	return out, nil
}

// column is one cell of a row, found by its header rather than by its
// position, so a table with an extra column reads the same.
func column(n specs.Name, header string) (string, bool) {
	for i, h := range n.Header {
		if strings.EqualFold(strings.TrimSpace(h), header) && i < len(n.Row) {
			return n.Row[i], true
		}
	}
	return "", false
}

// responses declares one answer per code of the deck, so an operation
// references rather than repeats them and a generator emits one type per
// code.
func responses(codes map[string]code) map[string]Response {
	out := map[string]Response{}
	for name, c := range codes {
		var statuses []string
		for _, s := range c.statuses {
			statuses = append(statuses, strconv.Itoa(s))
		}
		out[name] = Response{
			Description: fmt.Sprintf("%s %s: %s", strings.Join(statuses, " or "), name, c.sentence),
			Content:     map[string]MediaType{"application/json": {Schema: Schema{Ref: "#/components/schemas/Error"}}},
		}
	}
	return out
}

// schemas is the one shape every refusal carries, spec 003's envelope.
// A route's own request and response bodies are stated in prose by the
// spec that owns the route, and a schema invented from prose would claim
// types no spec states; the description of each operation carries the
// row instead.
func schemas() map[string]Schema {
	return map[string]Schema{
		"Error": {
			Type:        "object",
			Description: "The one refusal shape of this API. A caller branches on error.code and shows error.message.",
			Required:    []string{"error"},
			Properties: map[string]Schema{"error": {
				Type:     "object",
				Required: []string{"code", "message"},
				Properties: map[string]Schema{
					"code":    {Type: "string", Description: "The stable name of the row of the error table."},
					"message": {Type: "string", Description: "The one sentence of that row, fixed, to show a person."},
					"details": {Type: "object", Description: "The developer fields that row lists, in the developer register."},
				},
			}},
		},
	}
}

// operationID is a stable name for one route, derived from its method
// and path, so rewording a row does not move a generated client's method
// name.
func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	segments := 0
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		segments++
		last := 0
		for _, m := range wildcardRe.FindAllStringSubmatchIndex(seg, -1) {
			b.WriteString(title(seg[last:m[0]]))
			b.WriteString("By" + title(seg[m[2]:m[3]]))
			last = m[1]
		}
		b.WriteString(title(seg[last:]))
	}
	if segments == 0 {
		b.WriteString("Root")
	}
	return b.String()
}

// title upper-cases the first letter of each word of a segment and drops
// what an identifier cannot carry, so an operation id reads as one word
// per segment.
func title(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			upper = true
		case upper:
			b.WriteString(strings.ToUpper(string(r)))
			upper = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// JSON renders the document as indented JSON, which is what the YAML is
// converted from. It cannot fail: a Document is a closed set of strings,
// numbers, booleans, and slices and maps of those.
func (d *Document) JSON() []byte {
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		panic("apidoc: the document does not marshal: " + err.Error())
	}
	return append(out, '\n')
}

// YAML is the committed file's bytes: the comment header and the
// document, converted from the JSON so the key order is the struct's.
func (d *Document) YAML() ([]byte, error) {
	body, err := yaml.JSONToYAML(d.JSON())
	if err != nil {
		return nil, fmt.Errorf("render the document as YAML: %w", err)
	}
	return append([]byte(header), body...), nil
}

// Operations lists every "METHOD path" the document describes, sorted,
// for a test.
func (d *Document) Operations() []string {
	var out []string
	for path, item := range d.Paths {
		for method := range item {
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}
