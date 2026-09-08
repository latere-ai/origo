// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package sink_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/latere-ai/origo/test/stubs/sink"
)

func deliver(t *testing.T, s *sink.Server, signature, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", s.URL(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(sink.HeaderSignature, signature)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func control(t *testing.T, s *sink.Server, method, path, body string) (int, []byte) {
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
	repoA = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	pushA = `{"id":"e1","kind":"push","repo":"` + repoA + `","seq":1}`
)

// TestSignatureAndFailures is spec 013's criterion for the sink: a bad
// signature is refused, the configured failures are answered in order
// and then the sink recovers, and the control API lists and clears
// what arrived.
func TestSignatureAndFailures(t *testing.T) {
	s := sink.New(t)
	if s.Secret() != sink.DefaultSecret {
		t.Fatalf("secret %q", s.Secret())
	}
	headers := map[string]string{sink.HeaderEvent: "push", sink.HeaderDelivery: "e1"}
	// A bad signature and a missing one are refused and recorded as such.
	if status, _ := deliver(t, s, "sha256=00", pushA, headers); status != 401 {
		t.Fatalf("bad signature: %d", status)
	}
	if status, _ := deliver(t, s, "", pushA, headers); status != 401 {
		t.Fatalf("no signature: %d", status)
	}
	if got := s.Deliveries(repoA, "push"); len(got) != 2 || got[0].Verified || got[1].Verified || got[0].Status != 401 {
		t.Fatalf("refused deliveries: %+v", got)
	}
	// A good one is answered 200 and carries the headers and the body.
	if status, _ := deliver(t, s, sink.Sign(s.Secret(), []byte(pushA)), pushA, headers); status != 200 {
		t.Fatalf("good signature: %d", status)
	}
	got := s.Deliveries(repoA, "push")
	if len(got) != 3 || !got[2].Verified || got[2].ID != "e1" || got[2].Kind != "push" || got[2].Repo != repoA || string(got[2].Body) != pushA || got[2].Headers.Get(sink.HeaderDelivery) != "e1" {
		t.Fatalf("recorded delivery: %+v", got[2])
	}
	// The filters: another repository, another kind, and no filter.
	if len(s.Deliveries("other", "")) != 0 || len(s.Deliveries("", "deleted")) != 0 || len(s.Deliveries("", "")) != 3 {
		t.Fatal("filters")
	}
	// Fail(n, status): the next n answer the status, then 200 again.
	s.Clear()
	s.Fail(2, 500)
	sig := sink.Sign(s.Secret(), []byte(pushA))
	for i, want := range []int{500, 500, 200} {
		if status, _ := deliver(t, s, sig, pushA, headers); status != want {
			t.Fatalf("delivery %d: %d, want %d", i, status, want)
		}
	}
	// Fail(0, status) fails every one until cleared with a status of 0.
	s.Fail(0, 503)
	for range 3 {
		if status, _ := deliver(t, s, sig, pushA, headers); status != 503 {
			t.Fatalf("every: %d", status)
		}
	}
	s.Fail(0, 0)
	if status, _ := deliver(t, s, sig, pushA, headers); status != 200 {
		t.Fatal("not cleared")
	}
	// PUT /status with a body: the body is answered with the status for
	// count deliveries.
	if status, _ := control(t, s, "PUT", "/status", `{"status":429,"body":{"retry":true},"count":1}`); status != 204 {
		t.Fatalf("PUT /status: %d", status)
	}
	if status, raw := deliver(t, s, sig, pushA, headers); status != 429 || strings.TrimSpace(string(raw)) != `{"retry":true}` {
		t.Fatalf("configured answer: %d %s", status, raw)
	}
	if status, raw := deliver(t, s, sig, pushA, headers); status != 200 || len(raw) != 0 {
		t.Fatalf("after count: %d %s", status, raw)
	}
	if status, _ := control(t, s, "PUT", "/status", `{"status":99}`); status != 400 {
		t.Fatalf("bad status: %d", status)
	}
	if status, _ := control(t, s, "PUT", "/status", `nope`); status != 400 {
		t.Fatalf("malformed: %d", status)
	}
	if status, _ := control(t, s, "PUT", "/status", `{"status":500,"body":null,"count":0}`); status != 204 {
		t.Fatal("null body")
	}
	if status, raw := deliver(t, s, sig, pushA, headers); status != 500 || len(raw) != 0 {
		t.Fatalf("null body answer: %d %s", status, raw)
	}
	// GET and DELETE of /deliveries.
	status, raw := control(t, s, "GET", "/deliveries?repo="+repoA+"&kind=push", "")
	var listed []sink.Delivery
	if err := json.Unmarshal(raw, &listed); err != nil || status != 200 || len(listed) != 10 || listed[0].Status != 500 {
		t.Fatalf("GET /deliveries: %d %d %v", status, len(listed), err)
	}
	if status, raw := control(t, s, "GET", "/deliveries?kind=other", ""); status != 200 || strings.TrimSpace(string(raw)) != "null" {
		t.Fatalf("GET /deliveries of nothing: %d %s", status, raw)
	}
	if status, _ := control(t, s, "DELETE", "/deliveries", ""); status != 204 || len(s.Deliveries("", "")) != 0 {
		t.Fatal("DELETE /deliveries")
	}
}

func TestWaitAndOddBodies(t *testing.T) {
	s := sink.New(t)
	sig := sink.Sign(s.Secret(), []byte(pushA))
	go func() {
		time.Sleep(20 * time.Millisecond)
		deliver(t, s, sig, pushA, map[string]string{sink.HeaderEvent: "push"})
		deliver(t, s, sig, pushA, map[string]string{sink.HeaderEvent: "push"})
	}()
	if got, ok := s.Wait(repoA, "push", 2, 5*time.Second); !ok || len(got) != 2 {
		t.Fatalf("Wait: %v %d", ok, len(got))
	}
	if got, ok := s.Wait(repoA, "push", 3, 30*time.Millisecond); ok || len(got) != 2 {
		t.Fatalf("Wait timed out: %v %d", ok, len(got))
	}
	// The kind falls back to the body's when the header is absent, and a
	// body that is not JSON is recorded as a string.
	if status, _ := deliver(t, s, sig, pushA, nil); status != 200 {
		t.Fatal("no header")
	}
	if got := s.Deliveries(repoA, "push"); len(got) != 3 || got[2].Kind != "push" {
		t.Fatalf("kind from the body: %+v", got)
	}
	text := "not json"
	if status, _ := deliver(t, s, sink.Sign(s.Secret(), []byte(text)), text, nil); status != 200 {
		t.Fatal("text body")
	}
	if got := s.Deliveries("", ""); string(got[3].Body) != `"not json"` {
		t.Fatalf("text body recorded as %s", got[3].Body)
	}
	// A secret of one's own and a handler without a listener.
	other := sink.New(t, sink.WithSecret("k"))
	if status, _ := deliver(t, other, sink.Sign("k", []byte(pushA)), pushA, nil); status != 200 {
		t.Fatal("WithSecret")
	}
	h := sink.NewHandler()
	if h.Handler() == nil {
		t.Fatal("handler")
	}
	h.Close()
}
