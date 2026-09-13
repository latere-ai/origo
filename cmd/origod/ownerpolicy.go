// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"sort"

	"latere.ai/x/pkg/authz"

	"github.com/latere-ai/origo/internal/auth"
	"github.com/latere-ai/origo/internal/wal"
)

// walObjects is the built-in owner policy's view of the node's own
// repositories (Origo spec 028): whether a ref names a repository and the
// rendered subject that created it, read from the log's metadata. It runs
// only when no external authorizer is configured, so its cost, a metadata
// read per decision and an enumeration per directory page, is a
// self-hosted node's alone.
type walObjects struct {
	log *wal.Log
}

// Object resolves the ref to a repository and reports its creator. A ref
// that names nothing, whether by an unknown id or an unresolved name, is
// Object{Exists: false}: the policy treats it as not owned, which is also
// what lets a create through. ErrNotFound is that, not an outage; any
// other error is the outage, and the policy fails closed on it.
func (w walObjects) Object(ctx context.Context, ref auth.RepoRef) (authz.Object, error) {
	id := ref.ID
	if id == "" && ref.Owner != "" && ref.Slug != "" {
		resolved, err := w.log.Resolve(ctx, ref.Owner, ref.Slug)
		if errors.Is(err, wal.ErrNotFound) {
			return authz.Object{}, nil
		}
		if err != nil {
			return authz.Object{}, err
		}
		id = resolved
	}
	if id == "" {
		return authz.Object{}, nil
	}
	m, err := w.log.ReadMeta(ctx, id)
	if errors.Is(err, wal.ErrNotFound) {
		return authz.Object{}, nil
	}
	if err != nil {
		return authz.Object{}, err
	}
	return authz.Object{Exists: true, Owner: m.Creator}, nil
}

// Owned lists one page of the repositories the subject created, ordered
// by id so the cursor is stable across pages. The cursor is the last id
// of the previous page; the page holds the repositories whose id sorts
// after it, up to limit, and NextCursor is the last id when more remain.
func (w walObjects) Owned(ctx context.Context, subject, cursor string, limit int) (auth.Directory, error) {
	ids, err := w.log.Repos(ctx)
	if err != nil {
		return auth.Directory{}, err
	}
	sort.Strings(ids)
	var page []auth.DirectoryEntry
	for _, id := range ids {
		if id <= cursor {
			continue
		}
		m, err := w.log.ReadMeta(ctx, id)
		if errors.Is(err, wal.ErrNotFound) {
			continue
		}
		if err != nil {
			return auth.Directory{}, err
		}
		if m.Creator != subject || m.PurgedAt != nil {
			continue
		}
		if len(page) == limit {
			return auth.Directory{Repos: page, NextCursor: page[len(page)-1].ID}, nil
		}
		page = append(page, auth.DirectoryEntry{ID: m.ID, Owner: m.Owner, Slug: m.Slug})
	}
	return auth.Directory{Repos: page}, nil
}
