// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package limits

import "latere.ai/x/pkg/semaphore"

// Semaphore bounds admitted git subprocesses; nested helpers reuse their caller's slot.
type Semaphore = semaphore.Semaphore

// NewSemaphore creates n subprocess slots; nonpositive n disables the cap.
func NewSemaphore(n int) *Semaphore { return semaphore.New(n) }
