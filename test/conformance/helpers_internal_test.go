// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fatalRecorder is a testing.TB whose Fatalf records the message instead
// of ending the test, so a helper's failing path can be read.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) { r.fatal = fmt.Sprintf(format, args...) }

// TestAGroupNamesTheTargetFieldItNeeds: a skipped group says which
// Target field would have let it run.
func TestAGroupNamesTheTargetFieldItNeeds(t *testing.T) {
	for group, want := range map[string]string{
		GroupDelegation: "Issuer",
		GroupDeny:       "Authorizer",
		GroupQuota:      "Authorizer",
		GroupSource:     "Source, SourceToken, and SourceControl",
		GroupStorage:    "Fault",
		GroupRepository: "Fault",
	} {
		if got := groupField(group); got != want {
			t.Errorf("groupField(%q) = %q, want %q", group, got, want)
		}
	}
}

// TestACloneURLCredentialIsHidden: git's echo of a clone URL never
// carries the token, however many URLs it names, and text without one
// is left as it is.
func TestACloneURLCredentialIsHidden(t *testing.T) {
	for in, want := range map[string]string{
		"fatal: unable to access 'https://x:tok123@origo.example/r.git/'": "fatal: unable to access 'https://x:***@origo.example/r.git/'",
		"https://x:a@one.example and https://x:b@two.example":             "https://x:***@one.example and https://x:***@two.example",
		"nothing secret here":  "nothing secret here",
		"https://x:no-at-sign": "https://x:no-at-sign",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(redact(in), "tok123") {
			t.Errorf("redact(%q) kept the token", in)
		}
	}
}

// TestRetryAfterIsReadInSecondsWithASecondByDefault: a 429's
// Retry-After is waited out as given, and a missing, zero or malformed
// one waits a second.
func TestRetryAfterIsReadInSecondsWithASecondByDefault(t *testing.T) {
	for value, want := range map[string]time.Duration{"3": 3 * time.Second, "": time.Second, "0": time.Second, "soon": time.Second} {
		h := http.Header{}
		if value != "" {
			h.Set("Retry-After", value)
		}
		if got := retryAfter(h); got != want {
			t.Errorf("Retry-After %q = %v, want %v", value, got, want)
		}
	}
}

// TestAnUnverifiableAssertionIsRecordedByTheCase: an assertion the
// target's shape cannot make is listed under the case that skipped it.
func TestAnUnverifiableAssertionIsRecordedByTheCase(t *testing.T) {
	s := &session{}
	s.unverifiable(t, "event delivery", "no events sink")
	if len(s.unverified) != 1 || s.unverified[0] != t.Name()+": event delivery" {
		t.Fatalf("unverified = %q", s.unverified)
	}
}

// TestTheSuiteFailsThroughOnePlace: failIf and decode fail with the
// message and the body that broke them, and pass otherwise.
func TestTheSuiteFailsThroughOnePlace(t *testing.T) {
	r := &fatalRecorder{TB: t}
	failIf(r, false, "never %s", "shown")
	if r.fatal != "" {
		t.Fatalf("failIf failed on a false condition: %q", r.fatal)
	}
	failIf(r, true, "status %d", 500)
	if r.fatal != "status 500" {
		t.Fatalf("failIf message = %q", r.fatal)
	}

	r = &fatalRecorder{TB: t}
	var v struct{ Name string }
	decode(r, []byte(`{"Name":"repo"}`), &v)
	if r.fatal != "" || v.Name != "repo" {
		t.Fatalf("decode of a valid body: %q %+v", r.fatal, v)
	}
	decode(r, []byte(`{not json`), &v)
	if !strings.Contains(r.fatal, "{not json") {
		t.Fatalf("decode failure does not name the body: %q", r.fatal)
	}
}

// TestNoEventIsUnverifiedWithoutASink: a target without an events sink
// cannot show that no event arrived, so the case records it as
// unverified rather than passing it.
func TestNoEventIsUnverifiedWithoutASink(t *testing.T) {
	s := &session{}
	s.expectNoEvent(t, "repo-1", "push", 0)
	if len(s.unverified) != 1 || !strings.Contains(s.unverified[0], "the absence of a push event of repo-1") {
		t.Fatalf("unverified = %q", s.unverified)
	}
}
