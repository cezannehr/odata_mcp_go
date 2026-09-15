// Copyright (c) 2024 OData MCP Contributors
// SPDX-License-Identifier: MIT

package registry

import (
	"context"
	"testing"
	"time"
)

func TestForRebuildsABusyBridgeAfterTheMaxAge(t *testing.T) {
	var calls int64
	now := time.Now()
	clock := func() time.Time { return now }

	r := New(countingFactory(&calls), WithTTL(24*time.Hour), WithMaxAge(time.Hour), WithClock(clock))
	ctx := context.Background()

	first, err := r.For(ctx, testCreds("a"))
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}

	now = now.Add(50 * time.Minute)
	if again, _ := r.For(ctx, testCreds("a")); again != first || calls != 1 {
		t.Fatalf("a bridge under the max age was rebuilt: calls = %d", calls)
	}

	// Used 20 minutes ago, so the idle TTL is nowhere near; only the age applies.
	now = now.Add(20 * time.Minute)
	rebuilt, err := r.For(ctx, testCreds("a"))
	if err != nil {
		t.Fatalf("For() error = %v", err)
	}

	if rebuilt == first || calls != 2 {
		t.Errorf("bridge past the max age was not rebuilt: calls = %d", calls)
	}
	if closeable, ok := first.(*fakeBridge); !ok || !closeable.closed {
		t.Error("the replaced bridge was not closed")
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want the old entry replaced, not kept alongside", r.Len())
	}
}

func TestMakeRoomDropsAgedEntriesBeforeEvictingByUse(t *testing.T) {
	var calls int64
	now := time.Now()
	clock := func() time.Time { return now }

	r := New(countingFactory(&calls), WithTTL(24*time.Hour), WithMaxAge(time.Hour), WithClock(clock))
	ctx := context.Background()

	if _, err := r.For(ctx, testCreds("a")); err != nil {
		t.Fatalf("For() error = %v", err)
	}

	now = now.Add(2 * time.Hour)
	if _, err := r.For(ctx, testCreds("b")); err != nil {
		t.Fatalf("For() error = %v", err)
	}

	if r.Len() != 1 {
		t.Errorf("Len() = %d, want the aged entry dropped when another tenant arrived", r.Len())
	}
}

func TestDefaultMaxAgeIsTwoHours(t *testing.T) {
	if DefaultMaxAge != 2*time.Hour {
		t.Errorf("DefaultMaxAge = %s, want 2h", DefaultMaxAge)
	}
	if New(countingFactory(new(int64))).maxAge != DefaultMaxAge {
		t.Error("New() did not apply DefaultMaxAge")
	}
}
