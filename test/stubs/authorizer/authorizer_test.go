// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package authorizer_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/test/stubs/authorizer"
)

func call(t *testing.T, s *authorizer.Server, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", s.URL(), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func control(t *testing.T, s *authorizer.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, s.URL()+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

const (
	repoA = `{"id":"0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f","owner":"acme","slug":"app"}`
	repoB = `{"id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","owner":"","slug":""}`
)

func TestRulesAndProbe(t *testing.T) {
	s := authorizer.New(t)
	if s.Token() != authorizer.DefaultToken {
		t.Fatalf("token %q", s.Token())
	}
	// The bearer is required.
	if status, _ := call(t, s, "wrong", `{"subject":"alice","repo":`+repoA+`,"action":"read"}`); status != 401 {
		t.Fatalf("wrong bearer: %d", status)
	}
	if status, _ := call(t, s, s.Token(), "{"); status != 400 {
		t.Fatalf("malformed body: %d", status)
	}
	// The default allows every subject; the probe id is denied for all.
	if status, out := call(t, s, s.Token(), `{"subject":"alice","repo":`+repoA+`,"action":"admin"}`); status != 200 || out["allow"] != true || out["ttl"] != nil {
		t.Fatalf("default allow: %d %v", status, out)
	}
	if status, out := call(t, s, s.Token(), `{"subject":"alice","repo":{"id":"`+authorizer.ProbeID+`"},"action":"read"}`); status != 200 || out["allow"] != false || out["reason"] == "" {
		t.Fatalf("probe: %d %v", status, out)
	}
	// Rules: by id, by owner/slug, by actor; the later rule wins, and the
	// per-rule figures travel.
	s.Deny(authorizer.Rule{Subject: "alice", Repo: "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f", Action: "write"}, "read only")
	s.Allow(authorizer.Rule{Subject: "*", Repo: "acme/app", Action: "read", TTL: 5, Replicas: 3, QuotaBytes: 1024})
	s.Deny(authorizer.Rule{Subject: "svc", Actor: "svc", Action: "*"}, "no delegation")
	s.Allow(authorizer.Rule{Subject: "bob", Actor: "svc"})
	if _, out := call(t, s, s.Token(), `{"subject":"alice","repo":`+repoA+`,"action":"write"}`); out["allow"] != false || out["reason"] != "read only" {
		t.Fatalf("deny by id: %v", out)
	}
	if _, out := call(t, s, s.Token(), `{"subject":"carol","repo":`+repoA+`,"action":"read"}`); out["allow"] != true || out["ttl"] != 5.0 || out["replicas"] != 3.0 || out["quota_bytes"] != 1024.0 {
		t.Fatalf("allow by name with figures: %v", out)
	}
	if _, out := call(t, s, s.Token(), `{"subject":"svc","actor":"svc","repo":`+repoB+`,"action":"read"}`); out["allow"] != false || out["reason"] != "no delegation" {
		t.Fatalf("deny by actor: %v", out)
	}
	if _, out := call(t, s, s.Token(), `{"subject":"bob","actor":"svc","repo":`+repoB+`,"action":"admin"}`); out["allow"] != true {
		t.Fatalf("later allow wins: %v", out)
	}
	// Every request is recorded in order, listed and cleared by the
	// control API.
	reqs := s.Requests()
	if len(reqs) != 6 || reqs[2].Subject != "alice" || reqs[2].Action != "write" || reqs[3].Repo.Owner != "acme" || reqs[5].Actor != "svc" {
		t.Fatalf("requests: %+v", reqs)
	}
	status, raw := control(t, s, "GET", "/requests", "")
	var listed []authorizer.Request
	if err := json.Unmarshal(raw, &listed); err != nil || status != 200 || len(listed) != 6 {
		t.Fatalf("GET /requests: %d %s %v", status, raw, err)
	}
	if status, _ := control(t, s, "DELETE", "/requests", ""); status != 204 || len(s.Requests()) != 0 {
		t.Fatalf("DELETE /requests: %d, %d left", status, len(s.Requests()))
	}
	// PUT /rules replaces the table.
	if status, _ := control(t, s, "PUT", "/rules", `{"rules":[{"subject":"*","actor":"*","repo":"*","action":"*","allow":false,"reason":"closed"}]}`); status != 204 {
		t.Fatalf("PUT /rules: %d", status)
	}
	if _, out := call(t, s, s.Token(), `{"subject":"bob","actor":"svc","repo":`+repoB+`,"action":"admin"}`); out["allow"] != false || out["reason"] != "closed" {
		t.Fatalf("replaced table: %v", out)
	}
	if status, _ := control(t, s, "PUT", "/rules", `nope`); status != 400 {
		t.Fatalf("malformed rules: %d", status)
	}
	s.SetRules()
	if _, out := call(t, s, s.Token(), `{"subject":"bob","repo":`+repoB+`,"action":"admin"}`); out["allow"] != true {
		t.Fatalf("empty table falls back to the default: %v", out)
	}
	// A default that names subjects.
	named := authorizer.New(t, authorizer.WithAllow("dev"), authorizer.WithToken("t"))
	if _, out := call(t, named, "t", `{"subject":"dev","repo":`+repoB+`,"action":"read"}`); out["allow"] != true {
		t.Fatalf("named default: %v", out)
	}
	if _, out := call(t, named, "t", `{"subject":"eve","repo":`+repoB+`,"action":"read"}`); out["allow"] != false || !strings.Contains(out["reason"].(string), "eve") {
		t.Fatalf("subject outside the default: %v", out)
	}
	// Decide answers the same table in-process.
	var req authorizer.Request
	req.Subject, req.Action = "eve", "read"
	if rule := named.Decide(req); rule.Allow {
		t.Fatal("Decide allowed eve")
	}
}

func TestFailAndHang(t *testing.T) {
	s := authorizer.New(t)
	body := `{"subject":"alice","repo":` + repoA + `,"action":"read"}`
	s.Fail(500)
	if status, _ := call(t, s, s.Token(), body); status != 500 {
		t.Fatalf("Fail(500): %d", status)
	}
	s.Fail(0)
	if status, out := call(t, s, s.Token(), body); status != 200 || out["allow"] != true {
		t.Fatalf("Fail(0): %d %v", status, out)
	}
	s.Hang()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", s.URL(), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	if _, err := client.Do(req); err == nil {
		t.Fatal("a hung endpoint answered")
	}
	// The request was recorded even though it never got an answer.
	if len(s.Requests()) != 3 {
		t.Fatalf("requests: %d", len(s.Requests()))
	}
	s.Resume()
	if status, out := call(t, s, s.Token(), body); status != 200 || out["allow"] != true {
		t.Fatalf("after Resume: %d %v", status, out)
	}
	// A request hung at Close is released with a 503 rather than leaked.
	s.Hang()
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", s.URL(), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Requests()) < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.Close()
	s.Close()
	if code := <-done; code != 503 && code != 0 {
		t.Fatalf("released with %d", code)
	}
}

func TestHandlerServesWithoutAListener(t *testing.T) {
	s := authorizer.NewHandler(authorizer.WithToken("x"))
	if s.Handler() == nil || s.Token() != "x" {
		t.Fatal("handler")
	}
	s.Close()
}
