// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// Meta is what Origo stores about a repository beside its log: the id
// the consumer chose and the labels it uses in clone URLs. Origo never
// interprets owner or slug (spec 003).
type Meta struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrNameTaken reports an owner/slug pair another repository holds.
var ErrNameTaken = errors.New("wal: owner/slug is taken")

var (
	idRe    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ValidID reports whether id is a lower-case UUID.
func ValidID(id string) bool { return idRe.MatchString(id) }

// ValidLabel reports whether an owner or slug is a URL-safe label.
func ValidLabel(s string) bool { return labelRe.MatchString(s) && s != "." && s != ".." }

func (l *Log) metaKey(id string) string { return l.key(id, "meta") }

func (l *Log) nameKey(owner, slug string) string { return l.prefix + "names/" + owner + "/" + slug }

// CreateRepo records a new repository: its metadata, the name that
// resolves to it, and index/0 with HEAD on the default branch. Every
// object is created by create-if-absent, so a second create of the same
// id answers ErrExists and a taken name answers ErrNameTaken.
func (l *Log) CreateRepo(ctx context.Context, m Meta, defaultBranch string) (*Index, error) {
	if !ValidID(m.ID) || !ValidLabel(m.Owner) || !ValidLabel(m.Slug) || !ValidRefName("refs/heads/"+defaultBranch) {
		return nil, errors.New("wal: invalid repository id, owner, slug, or default branch")
	}
	now := l.now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if _, err := l.store.Create(ctx, l.metaKey(m.ID), BytesBody(data)); err != nil {
		return nil, err
	}
	if _, err := l.store.Create(ctx, l.nameKey(m.Owner, m.Slug), BytesBody([]byte(m.ID))); err != nil {
		_ = l.store.Delete(ctx, l.metaKey(m.ID))
		if errors.Is(err, ErrExists) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	ix := &Index{V: Version, Refs: map[string]string{"HEAD": "ref: refs/heads/" + defaultBranch}}
	body, err := EncodeIndex(ix)
	if err != nil {
		return nil, err
	}
	if _, err := l.store.Create(ctx, l.key(m.ID, IndexKey(0)), BytesBody(body)); err != nil && !errors.Is(err, ErrExists) {
		return nil, err
	}
	l.writeHint(ctx, m.ID, 0)
	return ix, nil
}

// ReadMeta fetches a repository's metadata, or ErrNotFound.
func (l *Log) ReadMeta(ctx context.Context, id string) (*Meta, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	rc, _, err := l.store.Get(ctx, l.metaKey(id), "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var m Meta
	if err := json.NewDecoder(io.LimitReader(rc, 64<<10)).Decode(&m); err != nil {
		return nil, fmt.Errorf("wal: meta: %w", err)
	}
	return &m, nil
}

// Rename moves a repository to a new owner/slug. The new name is claimed
// first by create-if-absent, so two renames onto one name cannot both
// succeed; the old name answers 404 once the alias is gone.
func (l *Log) Rename(ctx context.Context, id, owner, slug string) (*Meta, error) {
	if !ValidLabel(owner) || !ValidLabel(slug) {
		return nil, errors.New("wal: invalid owner or slug")
	}
	m, err := l.ReadMeta(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Owner == owner && m.Slug == slug {
		return m, nil
	}
	if _, err := l.store.Create(ctx, l.nameKey(owner, slug), BytesBody([]byte(id))); err != nil {
		if errors.Is(err, ErrExists) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	old := *m
	m.Owner, m.Slug, m.UpdatedAt = owner, slug, l.now().UTC()
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if _, err := l.store.Put(ctx, l.metaKey(id), BytesBody(data)); err != nil {
		return nil, err
	}
	if err := l.store.Delete(ctx, l.nameKey(old.Owner, old.Slug)); err != nil {
		l.logger.WarnContext(ctx, "old name not removed", "repo", id, "owner", old.Owner, "slug", old.Slug, "error", err)
	}
	return m, nil
}

// Resolve maps an owner/slug to the repository id, or ErrNotFound.
func (l *Log) Resolve(ctx context.Context, owner, slug string) (string, error) {
	if !ValidLabel(owner) || !ValidLabel(slug) {
		return "", ErrNotFound
	}
	rc, _, err := l.store.Get(ctx, l.nameKey(owner, slug), "")
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	id, err := io.ReadAll(io.LimitReader(rc, 64))
	if err != nil {
		return "", err
	}
	if !ValidID(string(id)) {
		return "", fmt.Errorf("wal: name %s/%s holds %q", owner, slug, id)
	}
	return string(id), nil
}
