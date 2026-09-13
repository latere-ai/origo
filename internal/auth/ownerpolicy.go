// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"fmt"

	"latere.ai/x/pkg/authz"
)

// The owner policy (Origo spec 028).
//
// With ORIGO_AUTHORIZER_URL unset the node needs no service to be usable:
// it runs the shared package's owner policy over its own repository
// metadata. A subject may create a repository, and may read, write, and
// administer one it created; a subject in ORIGO_ADMIN_SUBJECTS may do
// everything on every repository; the probe id and the anonymous subject
// are denied; repo.list returns the subject's own.
//
// The policy reads no claim. What it needs of a repository, whether it
// exists and the rendered subject that created it, it asks Objects for,
// so this file depends on no storage package and the node wires the
// lookup over its log.

// Objects looks a repository up for the owner policy.
type Objects interface {
	// Object reports whether the ref names a repository and, if so, the
	// rendered subject recorded as its creator. A ref that names nothing
	// is Object{Exists: false} and not an error.
	Object(ctx context.Context, ref RepoRef) (authz.Object, error)
	// Owned lists one page of the repositories the subject created, in a
	// stable order, with the cursor and the limit spec 026 pages by.
	Owned(ctx context.Context, subject, cursor string, limit int) (Directory, error)
}

// OwnerPolicy is the built-in authorizer. It satisfies Authorizer and
// Lister, so the guard treats it exactly as it treats a client.
type OwnerPolicy struct {
	policy  authz.Policy
	objects Objects
}

// NewOwnerPolicy builds the owner policy over the admin subjects and the
// object lookup. The one action that creates a repository is repo.admin
// on an id that does not yet exist, which is how the registration API
// admits a create.
func NewOwnerPolicy(admins []string, objects Objects) *OwnerPolicy {
	return &OwnerPolicy{
		policy:  authz.Policy{Admins: admins, Create: string(ActionAdmin)},
		objects: objects,
	}
}

// Authorize decides one request against the owner policy. A storage
// failure while looking the object up is an *Unavailable, so the policy
// fails closed exactly as a client does on an outage.
func (o *OwnerPolicy) Authorize(ctx context.Context, req authz.Request) (Decision, error) {
	obj, err := o.objects.Object(ctx, RepoRef{ID: req.Resource.ID, Owner: req.Resource.String("owner"), Slug: req.Resource.String("slug")})
	if err != nil {
		return Decision{}, &Unavailable{Err: fmt.Errorf("owner policy: %w", err)}
	}
	d := o.policy.Decide(req, obj)
	return Decision{
		Allow:      d.Allow,
		Reason:     d.Reason,
		TTL:        DefaultTTL,
		Replicas:   DefaultReplicas,
		QuotaBytes: DefaultQuotaBytes,
	}, nil
}

// List answers spec 026's directory question: the subject's own
// repositories. The probe and the anonymous subject reach here only
// through the guard, which never asks a list for a bound token; an
// anonymous subject is refused before the directory route, so an empty
// subject here lists nothing.
func (o *OwnerPolicy) List(ctx context.Context, req authz.Request) (Directory, error) {
	if req.Subject == "" {
		return Directory{Supported: true, Reason: authz.ReasonAnonymous}, nil
	}
	cursor := req.Resource.String("cursor")
	limit := req.Resource.Int("limit")
	if limit <= 0 {
		limit = DefaultListLimit
	}
	dir, err := o.objects.Owned(ctx, req.Subject, cursor, limit)
	if err != nil {
		return Directory{}, &Unavailable{Err: fmt.Errorf("owner policy: %w", err)}
	}
	dir.Supported = true
	dir.Allowed = true
	return dir, nil
}
