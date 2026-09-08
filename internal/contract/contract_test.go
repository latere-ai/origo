// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package contract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/httpjson"
)

func TestMiddlewareStampsTheContractVersion(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot || rec.Header().Get(Header) != Version {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get(Header))
	}
}

func TestEveryCodeHasOneSentenceInTheUserRegister(t *testing.T) {
	codes := Codes()
	if len(codes) != len(sentences) || len(codes) == 0 {
		t.Fatalf("Codes lists %d of %d", len(codes), len(sentences))
	}
	for _, c := range codes {
		s := Sentence(c)
		if !strings.HasSuffix(s, ".") || s[0] < 'A' || s[0] > 'Z' {
			t.Errorf("%s: %q is not a sentence", c, s)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an unknown code did not panic")
		}
	}()
	Sentence("nope")
}

func TestWriteRendersTheEnvelopeFromTheTable(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, http.StatusForbidden, CodeForbidden, map[string]any{"reason": "no"})
	var env httpjson.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 403 || env.Error.Code != CodeForbidden || env.Error.Message != Sentence(CodeForbidden) || env.Error.Details["reason"] != "no" {
		t.Fatalf("%d %+v", rec.Code, env)
	}
	if e := Error(CodeRepoNotFound, nil); e.Details != nil || e.Message != Sentence(CodeRepoNotFound) {
		t.Fatalf("empty details: %+v", e)
	}
}
