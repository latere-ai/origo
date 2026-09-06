// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"context"
	"io"
	"strings"
)

// prefixed namespaces a store under a prefix so a test runs in a shared
// bucket without meeting another test's keys.
type prefixed struct {
	Store
	prefix string
}

func (p *prefixed) Create(ctx context.Context, key string, body Body) (string, error) {
	return p.Store.Create(ctx, p.prefix+key, body)
}

func (p *prefixed) Put(ctx context.Context, key string, body Body) (string, error) {
	return p.Store.Put(ctx, p.prefix+key, body)
}

func (p *prefixed) Get(ctx context.Context, key, ifNoneMatch string) (io.ReadCloser, Object, error) {
	rc, o, err := p.Store.Get(ctx, p.prefix+key, ifNoneMatch)
	o.Key = strings.TrimPrefix(o.Key, p.prefix)
	return rc, o, err
}

func (p *prefixed) Head(ctx context.Context, key string) (Object, error) {
	o, err := p.Store.Head(ctx, p.prefix+key)
	o.Key = strings.TrimPrefix(o.Key, p.prefix)
	return o, err
}

func (p *prefixed) List(ctx context.Context, opts ListOptions) (ListResult, error) {
	opts.Prefix = p.prefix + opts.Prefix
	if opts.StartAfter != "" {
		opts.StartAfter = p.prefix + opts.StartAfter
	}
	res, err := p.Store.List(ctx, opts)
	for i := range res.Objects {
		res.Objects[i].Key = strings.TrimPrefix(res.Objects[i].Key, p.prefix)
	}
	for i := range res.Prefixes {
		res.Prefixes[i] = strings.TrimPrefix(res.Prefixes[i], p.prefix)
	}
	return res, err
}

func (p *prefixed) Delete(ctx context.Context, key string) error {
	return p.Store.Delete(ctx, p.prefix+key)
}
