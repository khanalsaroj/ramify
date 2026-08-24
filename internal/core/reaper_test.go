// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/internal/store"
)

func TestReaperSweepDestroysExpiredNotPinned(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reaper := NewReaper(h.store, h.rec, h.clock, nil)

	expiredEnv, err := h.rec.Apply(ctx, ApplyRequest{Project: "p", Branch: "expired", Subdomain: "expired", ArtifactRef: "r"})
	require.NoError(t, err)
	pinnedEnv, err := h.rec.Apply(ctx, ApplyRequest{Project: "p", Branch: "pinned", Subdomain: "pinned", ArtifactRef: "r"})
	require.NoError(t, err)
	futureEnv, err := h.rec.Apply(ctx, ApplyRequest{Project: "p", Branch: "future", Subdomain: "future", ArtifactRef: "r"})
	require.NoError(t, err)

	past := h.clock.now.Add(-time.Hour)
	future := h.clock.now.Add(time.Hour)

	expiredEnv.TTLExpiresAt = &past
	_, err = h.store.UpdateEnvironment(ctx, expiredEnv)
	require.NoError(t, err)

	pinnedEnv.TTLExpiresAt = &past
	pinnedEnv.Pinned = true
	_, err = h.store.UpdateEnvironment(ctx, pinnedEnv)
	require.NoError(t, err)

	futureEnv.TTLExpiresAt = &future
	_, err = h.store.UpdateEnvironment(ctx, futureEnv)
	require.NoError(t, err)

	require.NoError(t, reaper.Sweep(ctx))

	got, err := h.store.GetEnvironment(ctx, expiredEnv.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusDestroyed, got.Status, "expired, unpinned environment must be torn down")

	got, err = h.store.GetEnvironment(ctx, pinnedEnv.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusReady, got.Status, "pinned environment must not be swept even if past TTL")

	got, err = h.store.GetEnvironment(ctx, futureEnv.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusReady, got.Status, "environment with a future TTL must not be swept")
}

func TestReaperSweepNoExpiredEnvironments(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reaper := NewReaper(h.store, h.rec, h.clock, nil)

	require.NoError(t, reaper.Sweep(ctx))
}
