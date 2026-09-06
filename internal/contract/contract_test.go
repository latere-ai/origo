// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package contract

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestErrorEnvelopeAndHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusNotFound, CodeRepoNotFound, "repository not found")
	})).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 404 || rec.Header().Get(Header) != Version || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	var env Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code != CodeRepoNotFound || env.Error.Message == "" {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	rec = httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]int{"n": 1})
	if rec.Code != 201 || rec.Body.String() != `{"n":1}` {
		t.Fatalf("json: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, math.NaN())
	if rec.Code != 500 {
		t.Fatalf("unencodable value: %d", rec.Code)
	}
}
