// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package lfs

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A batch request comes from a git-lfs client this package does not
// control: names in any letter case are read.
func TestBatchRequestIsReadLeniently(t *testing.T) {
	oid := strings.Repeat("a", 64)
	body := `{"Operation":"download","objects":[{"OID":"` + oid + `","size":3}],"operation":"upload"}`
	var req batchRequest
	if err := decode(httptest.NewRequest("POST", "/batch", strings.NewReader(body)), &req); err != nil {
		t.Fatal(err)
	}
	if req.Operation != "upload" || len(req.Objects) != 1 || req.Objects[0].OID != oid || req.Objects[0].Size != 3 {
		t.Fatalf("%+v", req)
	}
}

// A body is one JSON object followed by a newline.
func TestBodyEndsInANewline(t *testing.T) {
	rec := httptest.NewRecorder()
	write(rec, 200, responseObject{OID: "o", Size: 1})
	if got, want := rec.Body.String(), `{"oid":"o","size":1}`+"\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
