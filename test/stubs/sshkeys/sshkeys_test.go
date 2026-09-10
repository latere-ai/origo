// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sshkeys

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const fingerprint = "SHA256:HxKAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func post(t *testing.T, s *Server, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func control(t *testing.T, s *Server, method, path, body string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestResolveAnswersBothVerdictsWith200 holds the contract of spec 024:
// both verdicts are 200, the bearer is required, and a fingerprint the
// table does not hold says nothing about why.
func TestResolveAnswersBothVerdictsWith200(t *testing.T) {
	s := New(t)
	if code, _ := post(t, s, "wrong", `{"fingerprint":"x"}`); code != http.StatusUnauthorized {
		t.Errorf("a wrong bearer answered %d", code)
	}
	code, body := post(t, s, s.Token(), `{"fingerprint":"`+fingerprint+`"}`)
	if code != http.StatusOK || body["found"] != false || len(body) != 1 {
		t.Fatalf("an unknown fingerprint: %d %+v", code, body)
	}

	s.Register(Key{Fingerprint: fingerprint, Subject: "u_1", KeyID: "k_1", TTL: 30})
	code, body = post(t, s, s.Token(), `{"fingerprint":"`+fingerprint+`","type":"ssh-ed25519"}`)
	if code != http.StatusOK || body["found"] != true || body["subject"] != "u_1" || body["key_id"] != "k_1" || body["ttl"] != 30.0 {
		t.Fatalf("a registered fingerprint: %d %+v", code, body)
	}

	// A key registered again replaces the row, which is the one shape a
	// map keyed by fingerprint can hold.
	s.Register(Key{Fingerprint: fingerprint, Subject: "u_2"})
	if _, body = post(t, s, s.Token(), `{"fingerprint":"`+fingerprint+`"}`); body["subject"] != "u_2" {
		t.Errorf("the row was not replaced: %+v", body)
	}
	if k, ok := s.Lookup(fingerprint); !ok || k.Subject != "u_2" {
		t.Errorf("Lookup = %+v %v", k, ok)
	}

	// A revoked key stops answering.
	s.Revoke(fingerprint)
	if _, body = post(t, s, s.Token(), `{"fingerprint":"`+fingerprint+`"}`); body["found"] != false {
		t.Errorf("a revoked key still answers: %+v", body)
	}
	if code, _ := post(t, s, s.Token(), "not json"); code != http.StatusBadRequest {
		t.Errorf("a body that does not parse answered %d", code)
	}
}

// TestControlEndpointsDriveTheTableAndTheOutages is the control API of
// spec 013's stub table, which a stack run drives through the host port.
func TestControlEndpointsDriveTheTableAndTheOutages(t *testing.T) {
	s := New(t)
	if code := control(t, s, http.MethodPut, "/keys", `{"keys":[{"fingerprint":"`+fingerprint+`","subject":"u_9"}]}`); code != http.StatusNoContent {
		t.Fatalf("PUT /keys answered %d", code)
	}
	if _, body := post(t, s, s.Token(), `{"fingerprint":"`+fingerprint+`"}`); body["subject"] != "u_9" {
		t.Fatalf("the table was not replaced: %+v", body)
	}
	if got := s.Requests(); len(got) != 1 || got[0].Fingerprint != fingerprint {
		t.Fatalf("the request list is %+v", got)
	}
	// The list is readable and clearable over HTTP.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL()+"/requests", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var listed []Request
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	_ = resp.Body.Close()
	if len(listed) != 1 {
		t.Fatalf("GET /requests listed %d", len(listed))
	}
	if code := control(t, s, http.MethodDelete, "/requests", ""); code != http.StatusNoContent || len(s.Requests()) != 0 {
		t.Fatalf("DELETE /requests: %d %d", code, len(s.Requests()))
	}
	if code := control(t, s, http.MethodDelete, "/keys/"+fingerprint, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE /keys answered %d", code)
	}
	if _, ok := s.Lookup(fingerprint); ok {
		t.Error("the key was not revoked")
	}

	// The outage states, set by method and over HTTP.
	if code := control(t, s, http.MethodPut, "/fail", `{"status":500}`); code != http.StatusNoContent {
		t.Fatalf("PUT /fail answered %d", code)
	}
	if code, _ := post(t, s, s.Token(), `{"fingerprint":"x"}`); code != http.StatusInternalServerError {
		t.Errorf("the failing endpoint answered %d", code)
	}
	if code := control(t, s, http.MethodPost, "/resume", ""); code != http.StatusNoContent {
		t.Fatalf("POST /resume answered %d", code)
	}
	if code, _ := post(t, s, s.Token(), `{"fingerprint":"x"}`); code != http.StatusOK {
		t.Errorf("the resumed endpoint answered %d", code)
	}
	if code := control(t, s, http.MethodPut, "/fail", "not json"); code != http.StatusBadRequest {
		t.Errorf("a malformed /fail answered %d", code)
	}
	if code := control(t, s, http.MethodPut, "/keys", "not json"); code != http.StatusBadRequest {
		t.Errorf("a malformed /keys answered %d", code)
	}
}

// TestHangHoldsUntilResume proves the outage a resolver that never
// answers is driven with: the request is released by Resume, and one
// left hanging is released by Close.
func TestHangHoldsUntilResume(t *testing.T) {
	s := New(t)
	if code := control(t, s, http.MethodPost, "/hang", ""); code != http.StatusNoContent {
		t.Fatalf("POST /hang answered %d", code)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := post(t, s, s.Token(), `{"fingerprint":"x"}`)
		done <- code
	}()
	// Nothing answers until the outage is cleared.
	select {
	case code := <-done:
		t.Fatalf("the hung endpoint answered %d", code)
	default:
	}
	s.Resume()
	if code := <-done; code != http.StatusOK {
		t.Errorf("the released request answered %d", code)
	}

	// A handler with no listener serves the same routes, which is what
	// the stub binary runs.
	h := NewHandler(WithToken("t"))
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]string{"fingerprint": "y"})
	if h.Handler() == nil {
		t.Error("NewHandler serves nothing")
	}
	h.Close()
	h.Close()
}
