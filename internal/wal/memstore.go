// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package wal

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the ETag S3 reports for a single-part object
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/wait"
)

// MemStore is an in-process Store with exactly the primitives spec 004
// relies on and the semantics the spike verified on MinIO and Spaces:
// Create refuses an existing key with ErrExists and leaves it untouched,
// Head answers ErrNotFound or the ETag, Get honours If-None-Match, List
// is lexical, and there is no compare-and-swap at all. The unit suite runs
// on it; the integration suite runs the same tests on MinIO.
type MemStore struct {
	mu      sync.Mutex
	objects map[string]memObject
	now     func() time.Time

	// Fault, when set, is consulted before every operation with the
	// method name and the key. A non-nil error is returned instead of
	// running the operation, except that Create and Put apply the write
	// first when the error is ErrLostResponse: that is the shape of a
	// request whose response was lost in transport.
	Fault func(op, key string) error

	// Calls counts operations by method name.
	Calls map[string]int

	// latency is what every operation waits under the caller's context
	// before it runs, the shape of a slow bucket (spec 015).
	latency time.Duration
}

// ErrLostResponse makes MemStore apply a write and then report a
// transport failure, so a test drives the read-back path of the commit.
var ErrLostResponse = fmt.Errorf("wal: response lost in transport")

type memObject struct {
	data     []byte
	etag     string
	modified time.Time
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{objects: map[string]memObject{}, now: time.Now, Calls: map[string]int{}}
}

// SetFault installs or clears the fault function under the lock, so a
// test changes it while a server goroutine is using the store.
func (m *MemStore) SetFault(f func(op, key string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Fault = f
}

// SetLatency makes every operation wait d under the caller's context
// before it runs, so a context that ends first fails the operation with
// its error the way a slow bucket fails a call under the storage
// deadline of spec 015. Zero clears it.
func (m *MemStore) SetLatency(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency = d
}

// SetClock replaces the clock that stamps LastModified.
func (m *MemStore) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Touch sets an object's LastModified, so a test ages an orphan.
func (m *MemStore) Touch(key string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objects[key]; ok {
		o.modified = at
		m.objects[key] = o
	}
}

// Keys reports every key in lexical order.
func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *MemStore) fault(ctx context.Context, op, key string) error {
	m.mu.Lock()
	m.Calls[op]++
	f, latency := m.Fault, m.latency
	m.mu.Unlock()
	if latency > 0 {
		if err := wait.Sleep(ctx, latency); err != nil {
			return err
		}
	}
	if f == nil {
		return nil
	}
	return f(op, key)
}

func (m *MemStore) write(key string, body Body, ifAbsent bool) (string, error) {
	data, err := body.ReadAll()
	if err != nil {
		return "", err
	}
	if body.Size >= 0 && int64(len(data)) != body.Size {
		return "", fmt.Errorf("wal: body size %d, declared %d", len(data), body.Size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.objects[key]; exists && ifAbsent {
		return "", ErrExists
	}
	sum := md5.Sum(data) //nolint:gosec // ETag
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	m.objects[key] = memObject{data: data, etag: etag, modified: m.now()}
	return etag, nil
}

// Create implements Store.
func (m *MemStore) Create(ctx context.Context, key string, body Body) (string, error) {
	err := m.fault(ctx, "Create", key)
	if err != nil && err != ErrLostResponse { //nolint:errorlint // sentinel identity on purpose
		return "", err
	}
	etag, werr := m.write(key, body, true)
	if err != nil && werr == nil {
		return "", err
	}
	return etag, werr
}

// Put implements Store.
func (m *MemStore) Put(ctx context.Context, key string, body Body) (string, error) {
	err := m.fault(ctx, "Put", key)
	if err != nil && err != ErrLostResponse { //nolint:errorlint // sentinel identity on purpose
		return "", err
	}
	etag, werr := m.write(key, body, false)
	if err != nil && werr == nil {
		return "", err
	}
	return etag, werr
}

// Get implements Store.
func (m *MemStore) Get(ctx context.Context, key, ifNoneMatch string) (io.ReadCloser, Object, error) {
	if err := m.fault(ctx, "Get", key); err != nil {
		return nil, Object{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, Object{}, ErrNotFound
	}
	if ifNoneMatch != "" && strings.Trim(ifNoneMatch, `"`) == strings.Trim(o.etag, `"`) {
		return nil, Object{}, ErrNotModified
	}
	return io.NopCloser(bytes.NewReader(o.data)), o.object(key), nil
}

// Head implements Store.
func (m *MemStore) Head(ctx context.Context, key string) (Object, error) {
	if err := m.fault(ctx, "Head", key); err != nil {
		return Object{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return Object{}, ErrNotFound
	}
	return o.object(key), nil
}

func (o memObject) object(key string) Object {
	return Object{Key: key, Size: int64(len(o.data)), ETag: o.etag, LastModified: o.modified}
}

// List implements Store.
func (m *MemStore) List(ctx context.Context, opts ListOptions) (ListResult, error) {
	if err := m.fault(ctx, "List", opts.Prefix); err != nil {
		return ListResult{}, err
	}
	max := opts.Max
	if max <= 0 || max > 1000 {
		max = 1000
	}
	var res ListResult
	seen := map[string]bool{}
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, key := range sortedKeys(m.objects) {
		if !strings.HasPrefix(key, opts.Prefix) || key <= opts.StartAfter {
			continue
		}
		if opts.Delimiter != "" {
			rest := key[len(opts.Prefix):]
			if i := strings.Index(rest, opts.Delimiter); i >= 0 {
				p := opts.Prefix + rest[:i+len(opts.Delimiter)]
				if !seen[p] {
					if count == max {
						res.Truncated = true
						break
					}
					seen[p] = true
					res.Prefixes = append(res.Prefixes, p)
					count++
				}
				continue
			}
		}
		if count == max {
			res.Truncated = true
			break
		}
		res.Objects = append(res.Objects, m.objects[key].object(key))
		count++
	}
	return res, nil
}

// Delete implements Store.
func (m *MemStore) Delete(ctx context.Context, key string) error {
	if err := m.fault(ctx, "Delete", key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func sortedKeys(m map[string]memObject) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
