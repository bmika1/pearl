// Copyright (c) 2025-2026 The Pearl Research Labs
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRetryBackoffGrowsToCap(t *testing.T) {
	b := newRetryBackoff(time.Second, 10*time.Second)

	var got []time.Duration
	for range 6 {
		got = append(got, b.delay())
	}

	require.Equal(t, []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 10 * time.Second, 10 * time.Second,
	}, got)

	b.reset()
	require.Equal(t, time.Second, b.delay())
}

func TestRetryBackoffWait(t *testing.T) {
	t.Run("waits for the delay", func(t *testing.T) {
		b := newRetryBackoff(20*time.Millisecond, time.Second)

		start := time.Now()
		require.True(t, b.wait(context.Background()))
		require.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
	})

	t.Run("returns early once the context ends", func(t *testing.T) {
		b := newRetryBackoff(time.Minute, time.Minute)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		require.False(t, b.wait(ctx))
		require.Less(t, time.Since(start), 10*time.Second)
	})
}
