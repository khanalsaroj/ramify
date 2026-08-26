// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/internal/core/policy"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// admissionHarness is newHarness with an admission policy applied.
func admissionHarness(t *testing.T, p policy.Policy, maxConcurrent int) *harness {
	t.Helper()
	h := newHarness(t)
	h.rec = NewReconciler(h.store, h.deploy, h.dns, h.cert, h.notify, h.clock,
		"preview.example.com", 0, nil, WithAdmission(p, maxConcurrent))
	return h
}

func pushEvent(branch string) providerapi.Event {
	return providerapi.Event{Kind: "branch_pushed", Project: "acme/web", Branch: branch, Artifact: "sha1"}
}

func exists(t *testing.T, h *harness, branch string) bool {
	t.Helper()
	_, err := h.store.GetEnvironmentByProjectBranch(context.Background(), "acme/web", branch)
	return err == nil
}

// A skipped event is not an error: the webhook already returned 202, and failing
// here would retry the event ten times and then dead-letter it for a branch the
// operator deliberately excluded.
func TestSkippedEventIsNotAnError(t *testing.T) {
	h := admissionHarness(t, policy.Policy{DenyBranches: []string{"wip/**"}}, 0)

	require.NoError(t, h.rec.ProcessWebhookEvent(context.Background(), pushEvent("wip/scratch")))
	require.False(t, exists(t, h, "wip/scratch"))
}

func TestAdmittedEventStillDeploys(t *testing.T) {
	h := admissionHarness(t, policy.Policy{DenyBranches: []string{"wip/**"}}, 0)

	require.NoError(t, h.rec.ProcessWebhookEvent(context.Background(), pushEvent("feat/login")))
	require.True(t, exists(t, h, "feat/login"))
}

// The default Reconciler must behave exactly as it did before admission existed.
func TestNoPolicyAdmitsEverything(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.rec.ProcessWebhookEvent(context.Background(), pushEvent("anything/at/all")))
	require.True(t, exists(t, h, "anything/at/all"))
}

func TestMaxConcurrentBlocksTheNextNewEnvironment(t *testing.T) {
	h := admissionHarness(t, policy.Policy{}, 2)
	ctx := context.Background()

	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, pushEvent("one")))
	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, pushEvent("two")))
	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, pushEvent("three")))

	require.True(t, exists(t, h, "one"))
	require.True(t, exists(t, h, "two"))
	require.False(t, exists(t, h, "three"))
}

// Dropping updates at the ceiling would freeze every live environment on a stale
// commit, which is worse than the resource pressure the ceiling exists to bound.
func TestMaxConcurrentStillUpdatesExistingEnvironments(t *testing.T) {
	h := admissionHarness(t, policy.Policy{}, 1)
	ctx := context.Background()

	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, pushEvent("one")))

	updated := pushEvent("one")
	updated.Artifact = "sha2"
	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, updated))

	env, err := h.store.GetEnvironmentByProjectBranch(ctx, "acme/web", "one")
	require.NoError(t, err)
	require.Equal(t, "sha2", env.ArtifactRef)
}

// A destroyed environment must release its slot, or the ceiling would be a
// one-way ratchet that permanently bricks the daemon.
func TestDestroyedEnvironmentFreesASlot(t *testing.T) {
	h := admissionHarness(t, policy.Policy{}, 1)
	ctx := context.Background()

	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, pushEvent("one")))
	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, pushEvent("two")))
	require.False(t, exists(t, h, "two"))

	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, providerapi.Event{
		Kind: "branch_deleted", Project: "acme/web", Branch: "one",
	}))

	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, pushEvent("two")))
	require.True(t, exists(t, h, "two"))
}

// The per-branch locks cannot enforce the ceiling: pushes to different branches
// take different locks, so without admissionMu each could observe the same
// pre-create count and every one of them would be admitted.
func TestMaxConcurrentHoldsUnderConcurrentPushesToDifferentBranches(t *testing.T) {
	const (
		limit    = 3
		branches = 25
	)
	h := admissionHarness(t, policy.Policy{}, limit)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range branches {
		wg.Go(func() {
			// Errors are ignored deliberately: this asserts on the ceiling, and
			// the fake providers are shared across goroutines.
			_ = h.rec.ProcessWebhookEvent(ctx, pushEvent(fmt.Sprintf("feat/b%02d", i)))
		})
	}
	wg.Wait()

	live, err := h.store.CountLiveEnvironments(ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, live, limit, "admission let more than max_concurrent_envs through")
}

// Label gating runs against the event the provider produced, so a Bitbucket
// event (which can never carry labels) must not be blocked by a label rule.
func TestLabelPolicyReachesTheReconciler(t *testing.T) {
	h := admissionHarness(t, policy.Policy{RequiredLabels: []string{"preview"}}, 0)
	ctx := context.Background()

	gated := providerapi.Event{
		Kind: "pr_synchronized", Project: "acme/web", Branch: "feat/gated",
		PRNumber: 1, Artifact: "sha1", LabelsKnown: true,
	}
	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, gated))
	require.False(t, exists(t, h, "feat/gated"))

	labeled := gated
	labeled.Branch = "feat/labeled"
	labeled.Labels = []string{"preview"}
	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, labeled))
	require.True(t, exists(t, h, "feat/labeled"))

	bitbucket := gated
	bitbucket.Branch = "feat/bitbucket"
	bitbucket.LabelsKnown = false
	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, bitbucket))
	require.True(t, exists(t, h, "feat/bitbucket"))
}

// Destroy must never be gated. A branch excluded by policy can still have an
// environment from before the rule was added, and blocking teardown would strand
// it until its TTL lapsed.
func TestDestroyIsNeverGated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, pushEvent("wip/scratch")))
	require.True(t, exists(t, h, "wip/scratch"))

	h.rec = NewReconciler(h.store, h.deploy, h.dns, h.cert, h.notify, h.clock,
		"preview.example.com", 0, nil,
		WithAdmission(policy.Policy{DenyBranches: []string{"wip/**"}}, 1))

	require.NoError(t, h.rec.ProcessWebhookEvent(ctx, providerapi.Event{
		Kind: "pr_closed", Project: "acme/web", Branch: "wip/scratch",
	}))

	env, err := h.store.GetEnvironmentByProjectBranch(ctx, "acme/web", "wip/scratch")
	require.NoError(t, err)
	require.Equal(t, store.StatusDestroyed, env.Status)
}
