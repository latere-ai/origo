// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/latere-ai/origo/internal/wal"
)

// Sign is the Origo-Signature value over a body under the secret.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// cursor is origo/events/<repo>/cursor: the highest push sequence
// delivered and the ids of the events without a sequence delivered in
// the last 24 hours.
type cursor struct {
	Seq       uint64      `json:"seq"`
	Delivered []delivered `json:"delivered"`
}

type delivered struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

func (c *cursor) has(id string) bool {
	return slices.ContainsFunc(c.Delivered, func(d delivered) bool { return d.ID == id })
}

// readCursor reads a repository's cursor; a missing one is empty.
func (d *Dispatcher) readCursor(ctx context.Context, repo string) (cursor, error) {
	var c cursor
	rc, _, err := d.store.Get(ctx, d.cursorKey(repo), "")
	if errors.Is(err, wal.ErrNotFound) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer func() { _ = rc.Close() }()
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&c); err != nil {
		return cursor{}, fmt.Errorf("cursor %s: %w", repo, err)
	}
	return c, nil
}

// advanceCursor records a delivery: a push raises seq when its sequence
// is higher, an event without one joins the delivered set, and entries
// older than the window are dropped on the rewrite.
func (d *Dispatcher) advanceCursor(ctx context.Context, env envelope) error {
	d.cursorMu.Lock()
	defer d.cursorMu.Unlock()
	c, err := d.readCursor(ctx, env.Repo)
	if err != nil {
		return err
	}
	if env.Kind == KindPush {
		c.Seq = max(c.Seq, env.Seq)
	} else if !c.has(env.ID) {
		c.Delivered = append(c.Delivered, delivered{ID: env.ID, At: env.At})
	}
	cut := d.now().Add(-Window)
	c.Delivered = slices.DeleteFunc(c.Delivered, func(x delivered) bool { return x.At.Before(cut) })
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = d.store.Put(ctx, d.cursorKey(env.Repo), wal.BytesBody(data))
	return err
}

// read fetches a pending object and its envelope.
func (d *Dispatcher) read(ctx context.Context, key string) (object, envelope, []byte, error) {
	rc, _, err := d.store.Get(ctx, key, "")
	if err != nil {
		return object{}, envelope{}, nil, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	if err != nil {
		return object{}, envelope{}, nil, err
	}
	var obj object
	if err := json.Unmarshal(raw, &obj); err != nil {
		return object{}, envelope{}, nil, fmt.Errorf("object %s: %w", key, err)
	}
	var env envelope
	if err := json.Unmarshal(obj.Event, &env); err != nil {
		return object{}, envelope{}, nil, fmt.Errorf("object %s: event: %w", key, err)
	}
	return obj, env, raw, nil
}

// post delivers one event and returns the status, 0 when no response
// arrived.
func (d *Dispatcher) post(ctx context.Context, body []byte, env envelope) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, DeliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSignature, Sign(d.secret, body))
	req.Header.Set(HeaderEvent, env.Kind)
	req.Header.Set(HeaderDelivery, env.ID)
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// unschedule drops key from the queue.
func (d *Dispatcher) unschedule(key string) {
	d.mu.Lock()
	delete(d.queue, key)
	d.mu.Unlock()
}

// reschedule sets key's due time after an attempt, whatever it was.
func (d *Dispatcher) reschedule(key string, due time.Time) {
	d.mu.Lock()
	d.queue[key] = due
	d.mu.Unlock()
}

// attempt delivers one pending object once and settles it: deleted and
// counted on a 2xx, rescheduled on the retry schedule otherwise, moved
// under the dead prefix once its at is older than the window.
func (d *Dispatcher) attempt(ctx context.Context, key string) {
	obj, env, raw, err := d.read(ctx, key)
	if errors.Is(err, wal.ErrNotFound) {
		// Delivered by another node, or by an earlier pass.
		d.unschedule(key)
		return
	}
	if err != nil {
		d.logger.WarnContext(ctx, "event object not read", "key", key, "error", err)
		d.reschedule(key, d.now().Add(Delay(1)))
		return
	}
	if isEmitKey(key) {
		c, err := d.readCursor(ctx, env.Repo)
		if err != nil {
			d.logger.WarnContext(ctx, "cursor not read", "key", key, "error", err)
			d.reschedule(key, d.now().Add(Delay(1)))
			return
		}
		if c.has(env.ID) {
			if err := d.store.Delete(ctx, key); err != nil {
				d.logger.WarnContext(ctx, "delivered event not deleted", "key", key, "error", err)
			}
			d.unschedule(key)
			return
		}
	}
	status, err := d.post(ctx, obj.Event, env)
	if status >= 200 && status < 300 {
		if err := d.store.Delete(ctx, key); err != nil {
			d.logger.WarnContext(ctx, "delivered event not deleted", "key", key, "error", err)
		}
		if err := d.advanceCursor(ctx, env); err != nil {
			d.logger.WarnContext(ctx, "cursor not advanced", "key", key, "error", err)
		}
		d.delivered.Inc(nil)
		d.unschedule(key)
		return
	}
	now := d.now()
	if now.Sub(env.At) > Window {
		if err := d.bury(ctx, key, raw); err != nil {
			d.logger.ErrorContext(ctx, "dead event not moved", "key", key, "error", err)
			d.reschedule(key, now.Add(Delay(len(Delays))))
			return
		}
		d.dead.Inc(nil)
		d.logger.WarnContext(ctx, "event dead-lettered", "key", key, "id", env.ID, "kind", env.Kind, "attempts", obj.Attempts+1, "status", status, "error", err)
		d.unschedule(key)
		return
	}
	obj.Attempts++
	obj.NextAt = now.Add(Delay(obj.Attempts)).UTC()
	data, _ := json.Marshal(obj)
	if _, err := d.store.Put(ctx, key, wal.BytesBody(data)); err != nil {
		d.logger.WarnContext(ctx, "event object not rewritten", "key", key, "error", err)
	}
	d.logger.InfoContext(ctx, "event delivery failed", "key", key, "id", env.ID, "kind", env.Kind, "attempt", obj.Attempts, "status", status, "error", err, "next_at", obj.NextAt)
	d.reschedule(key, obj.NextAt)
}

// bury moves an object under the dead prefix under its own key.
func (d *Dispatcher) bury(ctx context.Context, key string, raw []byte) error {
	if _, err := d.store.Put(ctx, d.deadKey(key), wal.BytesBody(raw)); err != nil {
		return err
	}
	return d.store.Delete(ctx, key)
}

// deliverDue attempts every queued key that is due and returns the
// earliest due time left, zero when the queue is empty.
func (d *Dispatcher) deliverDue(ctx context.Context) time.Time {
	now := d.now()
	d.mu.Lock()
	var due []string
	for key, at := range d.queue {
		if !at.After(now) {
			due = append(due, key)
		}
	}
	d.mu.Unlock()
	slices.Sort(due)
	for _, key := range due {
		if ctx.Err() != nil {
			return time.Time{}
		}
		d.attempt(ctx, key)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var next time.Time
	for _, at := range d.queue {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}

// pollInterval bounds the wait between passes, so a due time set on
// another clock than the loop's is never missed for long.
const pollInterval = 10 * time.Second

// runDeliver runs passes until ctx ends: after each, it waits for the
// next due time, a wake, or the poll interval, whichever comes first.
func (d *Dispatcher) runDeliver(ctx context.Context) {
	for {
		next := d.deliverDue(ctx)
		wait := pollInterval
		if !next.IsZero() {
			wait = min(wait, max(next.Sub(d.now()), 0))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-d.wake:
			t.Stop()
		case <-t.C:
		}
	}
}
