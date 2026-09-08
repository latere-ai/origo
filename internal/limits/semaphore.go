// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package limits

import (
	"context"
	"time"
)

// Semaphore bounds the git subprocesses running on a node at once
// (ORIGO_MAX_GIT_PROCS). One slot is admission control for the primary
// subprocess of a request or of a compaction step; a helper subprocess
// inside a request that already holds a slot takes none, because a
// nested acquire under a full semaphore would wait for a slot only the
// waiting request can free.
type Semaphore struct {
	slots chan struct{}
}

// NewSemaphore builds a semaphore of n slots. A value of zero or less
// takes the cap off and every acquire succeeds at once.
func NewSemaphore(n int) *Semaphore {
	if n <= 0 {
		return &Semaphore{}
	}
	return &Semaphore{slots: make(chan struct{}, n)}
}

// Size is the number of slots, zero when the semaphore is uncapped.
func (s *Semaphore) Size() int {
	if s == nil || s.slots == nil {
		return 0
	}
	return cap(s.slots)
}

// Held is the number of slots taken, for a test and for a log line.
func (s *Semaphore) Held() int {
	if s == nil || s.slots == nil {
		return 0
	}
	return len(s.slots)
}

// Acquire takes one slot, waiting at most d, and reports whether it
// got one. The release is idempotent. A nil semaphore, and one with no
// cap, grants at once, which is the compact.Slots seam of spec 006 on a
// node that configures no limit.
func (s *Semaphore) Acquire(ctx context.Context, d time.Duration) (func(), bool) {
	if s == nil || s.slots == nil {
		return func() {}, true
	}
	select {
	case s.slots <- struct{}{}:
		return s.release(), true
	default:
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		return s.release(), true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// release frees one slot once, however often the caller runs it.
func (s *Semaphore) release() func() {
	done := false
	return func() {
		if done {
			return
		}
		done = true
		<-s.slots
	}
}
