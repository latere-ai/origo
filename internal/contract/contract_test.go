// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package contract

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
