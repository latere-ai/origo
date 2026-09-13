// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"testing"

	"latere.ai/x/pkg/authz"
)

// fakeObjects is a repository lookup over a fixed map of id to creator.
type fakeObjects struct {
	owners map[string]string // id -> creating rendered subject
	err    error
}

func (f fakeObjects) Object(_ context.Context, ref RepoRef) (authz.Object, error) {
	if f.err != nil {
		return authz.Object{}, f.err
	}
	owner, ok := f.owners[ref.ID]
	if !ok {
		return authz.Object{}, nil
	}
	return authz.Object{Exists: true, Owner: owner}, nil
}

func (f fakeObjects) Owned(_ context.Context, subject, _ string, _ int) (Directory, error) {
	if f.err != nil {
		return Directory{}, f.err
	}
	var repos []DirectoryEntry
	for id, owner := range f.owners {
		if owner == subject {
			repos = append(repos, DirectoryEntry{ID: id})
		}
	}
	return Directory{Repos: repos}, nil
}

// probe builds a request for one subject, action, and repository id.
func ownerReq(subject, action, id string) authz.Request {
	return authz.Request{
		Subject:  subject,
		Action:   action,
		Resource: authz.NewResource(ResourceKind, id, nil),
	}
}

// TestOwnerPolicy is the fourth acceptance row of Origo spec 028: with no
// authorizer configured the owner policy holds every rule of its list,
// denies the probe and anonymous, and ORIGO_ADMIN_SUBJECTS acts on
// everything.
func TestOwnerPolicy(t *testing.T) {
	const (
		alice = "https://iss|alice"
		bob   = "https://iss|bob"
		admin = "https://iss|root"
		mine  = "0f5c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"
		yours = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
		fresh = "2b3c4d5e-6f70-4a8b-9c0d-1e2f3a4b5c6d"
	)
	objects := fakeObjects{owners: map[string]string{mine: alice, yours: bob}}
	p := NewOwnerPolicy([]string{admin}, objects)
	ctx := context.Background()

	rows := []struct {
		name    string
		subject string
		action  string
		id      string
		allow   bool
		reason  string
	}{
		{"owner reads own", alice, "repo.read", mine, true, ""},
		{"owner writes own", alice, "repo.write", mine, true, ""},
		{"owner administers own", alice, "repo.admin", mine, true, ""},
		{"non-owner denied", bob, "repo.read", mine, false, authz.ReasonNotOwner},
		{"owner creates a new id", alice, "repo.admin", fresh, true, ""},
		{"read of an absent id is not_owner", alice, "repo.read", fresh, false, authz.ReasonNotOwner},
		{"admin acts on another's repository", admin, "repo.admin", mine, true, ""},
		{"admin acts on an absent id", admin, "repo.read", fresh, true, ""},
		{"the probe is denied for the owner", alice, "repo.read", authz.ProbeID, false, authz.ReasonProbe},
		{"the probe is denied for an admin", admin, "repo.read", authz.ProbeID, false, authz.ReasonProbe},
		{"anonymous is denied", "", "repo.read", mine, false, authz.ReasonAnonymous},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			d, err := p.Authorize(ctx, ownerReq(row.subject, row.action, row.id))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Allow != row.allow || (!d.Allow && d.Reason != row.reason) {
				t.Fatalf("allow=%v reason=%q, want allow=%v reason=%q", d.Allow, d.Reason, row.allow, row.reason)
			}
			if d.Allow && (d.TTL != DefaultTTL || d.Replicas != DefaultReplicas || d.QuotaBytes != DefaultQuotaBytes) {
				t.Fatalf("an allow carries the defaults: %+v", d)
			}
		})
	}

	// repo.list returns the subject's own, and only its own.
	dir, err := p.List(ctx, listReq(alice, "", 0))
	if err != nil || !dir.Supported || !dir.Allowed || len(dir.Repos) != 1 || dir.Repos[0].ID != mine {
		t.Fatalf("alice's directory is %+v, %v", dir, err)
	}
	// The anonymous subject lists nothing, refused rather than paged.
	if dir, err := p.List(ctx, listReq("", "", 0)); err != nil || dir.Allowed {
		t.Fatalf("anonymous directory is %+v, %v", dir, err)
	}

	// A storage failure fails closed, as an *Unavailable.
	down := NewOwnerPolicy(nil, fakeObjects{err: errors.New("bucket down")})
	if _, err := down.Authorize(ctx, ownerReq(alice, "repo.read", mine)); err == nil {
		t.Fatal("a storage failure did not fail closed")
	} else if _, ok := errors.AsType[*Unavailable](err); !ok {
		t.Fatalf("the failure is not an *Unavailable: %v", err)
	}
	if _, err := down.List(ctx, listReq(alice, "", 0)); err == nil {
		t.Fatal("a storage failure in the directory did not fail closed")
	}
}
