// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedEnvironments creates n environments in one project, named b00..bNN so
// their creation order and branch order agree.
func seedEnvironments(t *testing.T, s Store, project string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		branch := fmt.Sprintf("b%02d", i)
		_, err := s.CreateEnvironment(ctx, Environment{
			Project: project, Branch: branch, Subdomain: branch,
			ArtifactRef: "r", Status: StatusPending,
		})
		require.NoError(t, err)
	}
}

func TestListEnvironmentsPaginates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedEnvironments(t, s, "acme/web", 10)

	first, err := s.ListEnvironments(ctx, ListOptions{Limit: 4})
	require.NoError(t, err)
	require.Len(t, first, 4)

	second, err := s.ListEnvironments(ctx, ListOptions{Limit: 4, Offset: 4})
	require.NoError(t, err)
	require.Len(t, second, 4)

	third, err := s.ListEnvironments(ctx, ListOptions{Limit: 4, Offset: 8})
	require.NoError(t, err)
	require.Len(t, third, 2, "final page holds the remainder")

	// Paging must partition the rows: no gaps, no repeats.
	seen := map[string]bool{}
	for _, page := range [][]Environment{first, second, third} {
		for _, env := range page {
			require.False(t, seen[env.ID], "environment %s returned on two pages", env.ID)
			seen[env.ID] = true
		}
	}
	require.Len(t, seen, 10)
}

func TestListEnvironmentsFiltersInSQL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedEnvironments(t, s, "acme/web", 3)
	seedEnvironments(t, s, "acme/api", 2)

	web, err := s.ListEnvironments(ctx, ListOptions{Project: "acme/web"})
	require.NoError(t, err)
	require.Len(t, web, 3)
	for _, env := range web {
		require.Equal(t, "acme/web", env.Project)
	}

	one, err := s.ListEnvironments(ctx, ListOptions{Project: "acme/api", Branch: "b01"})
	require.NoError(t, err)
	require.Len(t, one, 1)
	require.Equal(t, "b01", one[0].Branch)
}

// An empty ListOptions must not silently return everything, and an oversized
// limit must be clamped rather than honored.
func TestListOptionsLimitsAreClamped(t *testing.T) {
	require.Equal(t, DefaultListLimit, ListOptions{}.normalized().Limit)
	require.Equal(t, maxQueryLimit, ListOptions{Limit: MaxListLimit * 10}.normalized().Limit)
	// The lookahead row a paginating caller asks for must survive clamping.
	require.Equal(t, maxQueryLimit, ListOptions{Limit: MaxListLimit + 1}.normalized().Limit)
	require.Equal(t, 0, ListOptions{Offset: -5}.normalized().Offset)
	require.Equal(t, 7, ListOptions{Limit: 7}.normalized().Limit)
}

func TestListEnvironmentsAppliesDefaultLimit(t *testing.T) {
	s := newTestStore(t)
	seedEnvironments(t, s, "acme/web", DefaultListLimit+5)

	got, err := s.ListEnvironments(context.Background(), ListOptions{})
	require.NoError(t, err)
	require.Len(t, got, DefaultListLimit, "an unbounded request must still be windowed")
}
