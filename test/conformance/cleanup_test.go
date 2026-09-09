// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/latere-ai/origo/test/conformance"
	"github.com/latere-ai/origo/test/stubs/origo"
)

// get reads a repository and answers the status and the error code.
func get(t *testing.T, base, token, id string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", base+"/v1/repos/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body.Error.Code
}

// TestRunCleansUp is spec 021's criterion for a shared installation:
// after a run every id it created answers 404, and a repository created
// beside it under a conformance- slug by the test itself still answers
// 200, because Run deletes by id and never by prefix.
func TestRunCleansUp(t *testing.T) {
	s := origo.New(t)
	token := s.Token("dev", "")
	beside := "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
	req, _ := http.NewRequestWithContext(context.Background(), "POST", s.URL()+"/v1/repos",
		strings.NewReader(fmt.Sprintf(`{"id":%q,"owner":%q,"slug":"%sbeside"}`, beside, conformance.Owner, conformance.SlugPrefix)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("beside: %v %v", err, resp)
	}
	resp.Body.Close()

	report := conformance.Run(t, conformance.Target{
		URL: s.URL(), Token: token,
		Issuer: s.Issuer().URL(), Authorizer: s.Authorizer().URL(), EventsSink: s.Sink().URL(),
		Fault: s.Fault(), Skip: []string{"009/archive"},
	})
	if len(report.Failed) != 0 {
		t.Fatalf("failed: %v", report.Failed)
	}
	if len(report.Created) == 0 {
		t.Fatal("the run created nothing")
	}
	for _, id := range report.Created {
		if status, code := get(t, s.URL(), token, id); status != 404 || code != "repo_not_found" {
			t.Errorf("%s after the run: %d %s", id, status, code)
		}
	}
	if status, _ := get(t, s.URL(), token, beside); status != 200 {
		t.Errorf("the repository beside the run: %d", status)
	}
	// The Skip list is reported by name, and the source group skipped
	// itself.
	found := false
	for _, name := range report.Skipped {
		found = found || name == "009/archive"
	}
	if !found || len(report.SkippedGroups) != 1 || report.SkippedGroups[0] != conformance.GroupSource {
		t.Fatalf("skipped %v, groups %v", report.Skipped, report.SkippedGroups)
	}
}
