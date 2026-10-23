// Copyright (c) 2025-2026 The Pearl Research Labs
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"time"
)

// retryBackoff hands out exponentially growing delays, capped at a maximum.
type retryBackoff struct {
	first, longest time.Duration
	next           time.Duration
}

func newRetryBackoff(first, longest time.Duration) *retryBackoff {
	return &retryBackoff{first: first, longest: longest, next: first}
}

// delay returns how long to wait before the next attempt.
func (b *retryBackoff) delay() time.Duration {
	d := b.next
	b.next = min(2*b.next, b.longest)

	return d
}

func (b *retryBackoff) reset() {
	b.next = b.first
}

// wait sleeps for the next delay. It returns false if ctx ended first.
func (b *retryBackoff) wait(ctx context.Context) bool {
	timer := time.NewTimer(b.delay())
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
