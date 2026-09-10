// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net/http"
	"strings"

	versionpkg "github.com/latere-ai/origo/internal/version"
)

// The landing page of spec 022: what a person who opens an installation
// in a browser reads, in place of the credential dialog a 401 with
// WWW-Authenticate: Basic used to raise on the root. It is a sign on a
// door, not a web interface.
//
// The two bodies below carry the same facts in the same order. Nothing
// from the request or the configuration reaches either of them: the only
// value interpolated is the build's version, the same string GET
// /version serves, so the page is one constant document, identical for
// every visitor, and cannot be made to reflect a Host header, a query,
// or a path.

// projectURL is the one address the page links to.
const projectURL = "https://github.com/latere-ai/origo"

// versionMark is replaced by the build's version in both bodies.
const versionMark = "{version}"

const landingText = `Origo ` + versionMark + `

Git hosting as an infrastructure component. A push is an entry in a
write-ahead log in object storage, so any node can serve any repository.

This address is a git remote, not a website. There is nothing to browse
here.

Clone a repository:

    git clone https://{host}/{owner}/{slug}.git

{host} is the address you are reading this on and {owner}/{slug} names
the repository. Every repository also answers at /r/{id}.git, the form
that keeps working after a rename.

When git asks for a password, use any username and a bearer token from
this installation's identity provider as the password. A browser sign-in
box cannot accept one.

Documentation and source: ` + projectURL + `
`

// landingHTML is the same page for a browser. The stylesheet is inline
// and so is the icon: nothing is fetched from anywhere, and the icon
// link keeps a browser from asking for /favicon.ico, which the route
// beside this one answers for the browsers that ask anyway. Throw the
// stylesheet away and the document still reads top to bottom.
const landingHTML = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Origo</title>
<link rel="icon" href="data:,">
<style>
html { color-scheme: light dark }
body { max-width: 40rem; margin: 4rem auto; padding: 0 1.5rem;
       font-family: system-ui, sans-serif; line-height: 1.6 }
pre { padding: 0.75rem 1rem; overflow-x: auto; border: 1px solid gray }
footer { margin-top: 2.5rem; font-size: 0.875rem; opacity: 0.7 }
</style>
<h1>Origo</h1>
<p>Git hosting as an infrastructure component. A push is an entry in a
write-ahead log in object storage, so any node can serve any repository.</p>
<p><strong>This address is a git remote, not a website.</strong> There is
nothing to browse here.</p>
<p>Clone a repository:</p>
<pre>git clone https://{host}/{owner}/{slug}.git</pre>
<p><code>{host}</code> is the address you are reading this on and
<code>{owner}/{slug}</code> names the repository. Every repository also
answers at <code>/r/{id}.git</code>, the form that keeps working after a
rename.</p>
<p>When git asks for a password, use any username and a bearer token from
this installation's identity provider as the password. A browser sign-in
box cannot accept one.</p>
<p>Documentation and source:
<a href="` + projectURL + `">github.com/latere-ai/origo</a></p>
<footer>Origo ` + versionMark + `</footer>
`

// landing serves GET / without a token. A browser and a terminal browser
// ask for text/html and read the page; curl asks for anything and reads
// the same facts as lines, which is the form the other half of a git
// host's visitors are in. Vary: Accept goes on both so a cache in front
// keeps them apart.
func landing(w http.ResponseWriter, r *http.Request) {
	body, mediaType := landingText, "text/plain; charset=utf-8"
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		body, mediaType = landingHTML, "text/html; charset=utf-8"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Vary", "Accept")
	_, _ = io.WriteString(w, strings.ReplaceAll(body, versionMark, versionpkg.Version))
}

// favicon answers the request a browser makes on its own after it
// renders the page. Without it the request falls to the catch-all, the
// verifier refuses it 401 with WWW-Authenticate: Basic, and the dialog
// this page exists to remove appears a moment after the page does.
func favicon(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
