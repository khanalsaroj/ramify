// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// RunDeployProviderContract verifies the minimum behavior every
// providerapi.DeployProvider implementation must satisfy: create, healthy,
// update-in-place without creating a duplicate resource, destroy, and idempotent
// destroy.
func RunDeployProviderContract(t *testing.T, p providerapi.DeployProvider) {
	t.Helper()
	ctx := context.Background()

	spec := providerapi.EnvSpec{
		Project:     "contract/project",
		Branch:      "contract-branch",
		Subdomain:   "contract-branch",
		ArtifactRef: "ref-v1",
	}

	dep, err := p.Apply(ctx, spec)
	require.NoError(t, err)
	require.NotEmpty(t, dep.Ref)

	t.Run("healthy after create", func(t *testing.T) {
		status, err := p.HealthCheck(ctx, dep.Ref)
		require.NoError(t, err)
		require.True(t, status.Healthy)
	})

	t.Run("update in place creates no duplicate resource", func(t *testing.T) {
		spec2 := spec
		spec2.ArtifactRef = "ref-v2"
		spec2.PreviousRef = dep.Ref
		dep2, err := p.Apply(ctx, spec2)
		require.NoError(t, err)
		require.Equal(t, dep.Ref, dep2.Ref, "updating an existing environment must reuse the same ref, not create a duplicate resource")

		status, err := p.HealthCheck(ctx, dep2.Ref)
		require.NoError(t, err)
		require.True(t, status.Healthy)
	})

	t.Run("destroy", func(t *testing.T) {
		require.NoError(t, p.Destroy(ctx, dep.Ref))
	})

	t.Run("destroy is idempotent", func(t *testing.T) {
		require.NoError(t, p.Destroy(ctx, dep.Ref))
	})
}
