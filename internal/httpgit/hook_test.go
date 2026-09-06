// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package httpgit

import (
	"testing"
	"time"
)

// git can exit before the hook runs, and before the goroutine reading
// the hook's updates has opened the FIFO. A single release then finds
// no reader, the reader's blocking open never returns, and the handler
// waits forever. drain repeats the release until the reader is found.
func TestDrainReachesAReaderThatOpensLate(t *testing.T) {
	ch, err := newHookChannel(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch.close()
	if ch.release() {
		t.Fatal("release found a reader before one existed")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(30 * time.Millisecond)
		if _, ok, err := ch.readUpdates(); ok || err != nil {
			t.Errorf("late reader: ok %v, %v", ok, err)
		}
	}()
	finished := make(chan struct{})
	go func() { ch.drain(done); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not reach the late reader")
	}
	// A reader that already finished is not waited for.
	ch2, err := newHookChannel(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ch2.close()
	closed := make(chan struct{})
	close(closed)
	ch2.drain(closed)
}
