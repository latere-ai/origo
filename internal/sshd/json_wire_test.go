// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The request to the operator's endpoint has a fixed layout, and its
// answer is read leniently: a name in another letter case, a repeated
// name and invalid UTF-8 in a value are accepted.
func TestKeysEndpointWire(t *testing.T) {
	signer, err := ssh.NewSignerFromKey(generateKey(t))
	if err != nil {
		t.Fatal(err)
	}
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "{\"Found\":false,\"found\":true,\"SUBJECT\":\"iss|a\xff\",\"ttl\":30}")
	}))
	defer srv.Close()
	res, err := NewResolver(ResolverOptions{URL: srv.URL, Token: "t", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := res.Resolve(t.Context(), signer.PublicKey())
	if err != nil || !answer.Found || answer.Subject != "iss|a�" {
		t.Fatalf("%+v %v", answer, err)
	}
	want := RequestFor(signer.PublicKey())
	if string(sent) != `{"fingerprint":"`+want.Fingerprint+`","type":"`+want.Type+`","public_key":"`+want.PublicKey+`"}` {
		t.Fatalf("request %s", sent)
	}
}
