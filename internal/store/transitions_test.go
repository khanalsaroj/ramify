// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanTransition(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		{StatusPending, StatusDeploying, true},
		{StatusDeploying, StatusReady, true},
		{StatusDeploying, StatusFailed, true},
		{StatusReady, StatusDeploying, true},  // a new push redeploys
		{StatusFailed, StatusDeploying, true}, // a retry after failure
		{StatusDestroying, StatusDestroyed, true},
		{StatusDestroyed, StatusDeploying, true}, // branch pushed again after teardown
		{StatusReady, StatusReady, true},         // TTL refresh touches no status

		{StatusDestroyed, StatusReady, false},  // teardown cannot un-happen
		{StatusPending, StatusReady, false},    // must deploy first
		{StatusDestroying, StatusReady, false}, // mid-teardown cannot go live
		{StatusDestroyed, StatusSleeping, false},
		{"bogus", StatusReady, false}, // unknown source status
		{StatusReady, "bogus", false}, // unknown target status
		{"bogus", "bogus", false},     // unknown status is not self-consistent
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s_to_%s", tc.from, tc.to), func(t *testing.T) {
			require.Equal(t, tc.want, CanTransition(tc.from, tc.to))
		})
	}
}

// The store must reject an impossible lifecycle move rather than writing it,
// so a bug in one caller cannot corrupt an environment's state for everyone.
func TestUpdateEnvironmentRejectsInvalidTransition(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env, err := s.CreateEnvironment(ctx, Environment{
		Project: "acme/web", Branch: "main", Subdomain: "main",
		ArtifactRef: "r", Status: StatusPending,
	})
	require.NoError(t, err)

	env.Status = StatusReady // pending -> ready skips deploying
	_, err = s.UpdateEnvironment(ctx, env)
	require.ErrorIs(t, err, ErrInvalidTransition)

	// The rejected write must not have landed.
	got, err := s.GetEnvironment(ctx, env.ID)
	require.NoError(t, err)
	require.Equal(t, StatusPending, got.Status)
}

func TestUpdateEnvironmentAllowsLegalTransition(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env, err := s.CreateEnvironment(ctx, Environment{
		Project: "acme/web", Branch: "main", Subdomain: "main",
		ArtifactRef: "r", Status: StatusPending,
	})
	require.NoError(t, err)

	for _, next := range []string{StatusDeploying, StatusReady, StatusDestroying, StatusDestroyed} {
		env.Status = next
		env, err = s.UpdateEnvironment(ctx, env)
		require.NoError(t, err, "transition to %s must be allowed", next)
		require.Equal(t, next, env.Status)
	}
}

// A same-status update carrying other field changes must be accepted: the reaper
// and TTL refresh rely on it.
func TestUpdateEnvironmentAllowsSameStatusFieldUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env, err := s.CreateEnvironment(ctx, Environment{
		Project: "acme/web", Branch: "main", Subdomain: "main",
		ArtifactRef: "r", Status: StatusPending,
	})
	require.NoError(t, err)

	env.Pinned = true
	env.DeployRef = "compose-123"
	updated, err := s.UpdateEnvironment(ctx, env)
	require.NoError(t, err)
	require.True(t, updated.Pinned)
	require.Equal(t, "compose-123", updated.DeployRef)
}
