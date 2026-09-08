// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/latere-ai/origo/internal/contract"
)

// Denied is a refused request: the authorizer's reason, or the scope of
// a repository-bound token.
type Denied struct {
	Subject string
	Action  Action
	Reason  string
}

func (d *Denied) Error() string { return "auth: denied: " + d.Reason }

// The reasons a repository-bound token is refused with.
const (
	ReasonOtherRepository = "token bound to another repository"
	ReasonScope           = "token scope does not allow this action"
)

// Guard decides requests: a repository-bound token by its scope, every
// other principal by the authorizer.
type Guard struct {
	authorizer Authorizer
	logger     *slog.Logger
}

// NewGuard builds a guard over the authorizer.
func NewGuard(a Authorizer, logger *slog.Logger) *Guard {
	if logger == nil {
		logger = slog.Default()
	}
	return &Guard{authorizer: a, logger: logger}
}

// Authorize decides one request for the principal. It returns nil on an
// allow, a *Denied on a deny, and an *Unavailable when no decision
// could be made.
func (g *Guard) Authorize(ctx context.Context, p Principal, repo RepoRef, action Action) error {
	_, err := g.Decide(ctx, p, repo, action)
	return err
}

// Decide is Authorize with the decision behind the allow, for a handler
// that reads a field of it: the LFS batch reads QuotaBytes (spec 010).
// A repository-bound token is decided by its own scope and the
// authorizer never sees it, so the decision it yields carries the
// defaults of spec 007's table.
func (g *Guard) Decide(ctx context.Context, p Principal, repo RepoRef, action Action) (Decision, error) {
	if b := p.Bound; b != nil {
		if repo.ID == "" || repo.ID != b.Repo {
			return Decision{}, &Denied{Subject: p.Subject, Action: action, Reason: ReasonOtherRepository}
		}
		if !b.Scope.allows(action) {
			return Decision{}, &Denied{Subject: p.Subject, Action: action, Reason: ReasonScope}
		}
		return Decision{Allow: true, TTL: DefaultTTL, Replicas: DefaultReplicas, QuotaBytes: DefaultQuotaBytes}, nil
	}
	d, err := g.authorizer.Authorize(ctx, Request{Subject: p.Subject, Actor: p.Actor, Repo: repo, Action: action})
	if err != nil {
		return Decision{}, err
	}
	if !d.Allow {
		return Decision{}, &Denied{Subject: p.Subject, Action: action, Reason: d.Reason}
	}
	return d, nil
}

// allows is spec 007's scope rule: read allows read, write allows read
// and write, neither allows admin.
func (s Scope) allows(action Action) bool {
	switch action {
	case ActionRead:
		return s == ScopeRead || s == ScopeWrite
	case ActionWrite:
		return s == ScopeWrite
	}
	return false
}

// Allow decides the request for the principal it carries and writes the
// refusal when there is one: 403 forbidden for a deny with the action,
// the subject, and the reason in details, 503 authorizer_unavailable
// when no decision could be made. It reports whether the handler may go
// on.
func (g *Guard) Allow(w http.ResponseWriter, r *http.Request, repo RepoRef, action Action) bool {
	err := g.Authorize(r.Context(), FromContext(r.Context()), repo, action)
	if err == nil {
		return true
	}
	WriteRefusal(w, r, err, g.logger)
	return false
}

// WriteRefusal renders a *Denied or an *Unavailable.
func WriteRefusal(w http.ResponseWriter, r *http.Request, err error, logger *slog.Logger) {
	if denied, ok := errors.AsType[*Denied](err); ok {
		contract.Write(w, http.StatusForbidden, contract.CodeForbidden, map[string]any{
			"action": string(denied.Action), "subject": denied.Subject, "reason": denied.Reason,
		})
		return
	}
	details := map[string]any{"error": err.Error()}
	if u, ok := errors.AsType[*Unavailable](err); ok {
		details["url"] = u.URL
		if u.Status != 0 {
			details["status"] = u.Status
		}
		if u.Err != nil {
			details["error"] = u.Err.Error()
		} else {
			delete(details, "error")
		}
	}
	if logger != nil {
		logger.ErrorContext(r.Context(), "authorizer unavailable", "path", r.URL.Path, "error", err)
	}
	contract.Write(w, http.StatusServiceUnavailable, contract.CodeAuthorizerUnavailable, details)
}
