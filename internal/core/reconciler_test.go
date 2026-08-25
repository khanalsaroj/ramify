// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/test/fakes"
)

// fakeClock is a Clock whose Sleep is a no-op that just counts calls, so backoff
// tests run instantly.
type fakeClock struct {
	now        time.Time
	sleepCalls int
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(time.Duration) {
	c.sleepCalls++
}

type harness struct {
	store  store.Store
	deploy *fakes.DeployProvider
	dns    *fakes.DNSProvider
	cert   *fakes.CertificateProvider
	notify *fakes.NotifierProvider
	clock  *fakeClock
	rec    *Reconciler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	h := &harness{
		store:  st,
		deploy: fakes.NewDeployProvider(),
		dns:    fakes.NewDNSProvider(),
		cert:   fakes.NewCertificateProvider(),
		notify: fakes.NewNotifierProvider(),
		clock:  &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	h.rec = NewReconciler(h.store, h.deploy, h.dns, h.cert, h.notify, h.clock, "preview.example.com", 0, nil)
	return h
}

func TestApplyCreatesEnvironment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	env, err := h.rec.Apply(ctx, ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1", PRNumber: 42})
	require.NoError(t, err)
	require.Equal(t, store.StatusReady, env.Status)
	require.NotEmpty(t, env.DeployRef)

	records, err := h.store.ListDNSRecords(ctx, env.ID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "feature-x.preview.example.com", records[0].Name)

	require.Len(t, h.notify.Notifications, 1)
	require.Equal(t, "ready", h.notify.Notifications[0].Event.Kind)
}

func TestApplyIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	req := ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"}
	env1, err := h.rec.Apply(ctx, req)
	require.NoError(t, err)

	env2, err := h.rec.Apply(ctx, req)
	require.NoError(t, err)

	require.Equal(t, env1.ID, env2.ID)
	require.Equal(t, env1.DeployRef, env2.DeployRef)
	require.Equal(t, 2, h.deploy.ApplyCount(env1.DeployRef))

	records, err := h.store.ListDNSRecords(ctx, env1.ID)
	require.NoError(t, err)
	require.Len(t, records, 1, "no duplicate DNS record on repeated Apply")
}

func TestApplyUpdateWithNewArtifactRefNoDuplicateDNS(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	env1, err := h.rec.Apply(ctx, ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"})
	require.NoError(t, err)

	env2, err := h.rec.Apply(ctx, ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref2"})
	require.NoError(t, err)

	require.Equal(t, env1.ID, env2.ID)
	require.Equal(t, "ref2", env2.ArtifactRef)
	require.Equal(t, env1.DeployRef, env2.DeployRef, "update reuses the same deploy ref")

	records, err := h.store.ListDNSRecords(ctx, env1.ID)
	require.NoError(t, err)
	require.Len(t, records, 1)

	require.Len(t, h.notify.Notifications, 2)
	require.Equal(t, "ready", h.notify.Notifications[0].Event.Kind)
	require.Equal(t, "updated", h.notify.Notifications[1].Event.Kind)
}

func TestApplyRetriesThenFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deploy.ApplyErr = errors.New("boom")

	_, err := h.rec.Apply(ctx, ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"})
	require.Error(t, err)

	require.Equal(t, maxApplyAttempts, h.deploy.ApplyCalls())
	require.Equal(t, maxApplyAttempts-1, h.clock.sleepCalls)

	env, err := h.store.GetEnvironmentByProjectBranch(ctx, "acme/web", "feature-x")
	require.NoError(t, err)
	require.Equal(t, store.StatusFailed, env.Status)

	require.Len(t, h.notify.Notifications, 1)
	require.Equal(t, "failed", h.notify.Notifications[0].Event.Kind)
}

func TestDestroyTearsDownInOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	env, err := h.rec.Apply(ctx, ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"})
	require.NoError(t, err)

	require.NoError(t, h.rec.Destroy(ctx, env))

	final, err := h.store.GetEnvironment(ctx, env.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusDestroyed, final.Status)

	records, err := h.store.ListDNSRecords(ctx, env.ID)
	require.NoError(t, err)
	require.Empty(t, records)

	status, err := h.deploy.HealthCheck(ctx, env.DeployRef)
	require.NoError(t, err)
	require.False(t, status.Healthy)
	require.Equal(t, "destroyed", status.Detail)
}

func TestDestroyIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	env, err := h.rec.Apply(ctx, ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"})
	require.NoError(t, err)

	require.NoError(t, h.rec.Destroy(ctx, env))

	final, err := h.store.GetEnvironment(ctx, env.ID)
	require.NoError(t, err)

	require.NoError(t, h.rec.Destroy(ctx, final))
}

func TestReplayUnprocessedEventsRecoversCrashedApply(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Simulate a crash: an environment and an apply_requested event were
	// persisted, but the process died before any provider call completed.
	env, err := h.store.CreateEnvironment(ctx, store.Environment{
		Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1", Status: store.StatusPending,
	})
	require.NoError(t, err)

	payload, err := marshalApplyPayload(ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"})
	require.NoError(t, err)
	_, err = h.store.CreateEvent(ctx, store.Event{EnvironmentID: env.ID, Kind: EventKindApplyRequested, Payload: payload})
	require.NoError(t, err)

	unprocessed, err := h.store.ListUnprocessedEvents(ctx)
	require.NoError(t, err)
	require.Len(t, unprocessed, 1)

	require.NoError(t, h.rec.ReplayUnprocessedEvents(ctx))

	recovered, err := h.store.GetEnvironment(ctx, env.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusReady, recovered.Status)
	require.NotEmpty(t, recovered.DeployRef)

	unprocessed, err = h.store.ListUnprocessedEvents(ctx)
	require.NoError(t, err)
	require.Empty(t, unprocessed)
}

func TestReplayUnprocessedEventsRecoversCrashedDestroy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	env, err := h.rec.Apply(ctx, ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"})
	require.NoError(t, err)

	env.Status = store.StatusDestroying
	env, err = h.store.UpdateEnvironment(ctx, env)
	require.NoError(t, err)

	payload, err := marshalDestroyPayload(env.Project, env.Branch)
	require.NoError(t, err)
	_, err = h.store.CreateEvent(ctx, store.Event{EnvironmentID: env.ID, Kind: EventKindDestroyRequested, Payload: payload})
	require.NoError(t, err)

	require.NoError(t, h.rec.ReplayUnprocessedEvents(ctx))

	final, err := h.store.GetEnvironment(ctx, env.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusDestroyed, final.Status)
}

func TestReplayFailureRemainsPendingAndSchedulesRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deploy.ApplyErr = errors.New("temporary provider outage")

	env, err := h.store.CreateEnvironment(ctx, store.Environment{
		Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1", Status: store.StatusPending,
	})
	require.NoError(t, err)
	payload, err := marshalApplyPayload(ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"})
	require.NoError(t, err)
	_, err = h.store.CreateEvent(ctx, store.Event{EnvironmentID: env.ID, Kind: EventKindApplyRequested, Payload: payload})
	require.NoError(t, err)

	require.NoError(t, h.rec.ReplayUnprocessedEvents(ctx))
	events, err := h.store.ListUnprocessedEvents(ctx)
	require.NoError(t, err)
	require.Len(t, events, 1, "failed replay must remain durable")
	require.Equal(t, 1, events[0].Attempts)
	require.NotNil(t, events[0].NextAttemptAt)
	require.Contains(t, events[0].LastError, "temporary provider outage")
}

func TestApplySetsAndRefreshesTTLWhenConfigured(t *testing.T) {
	h := newHarness(t)
	rec := NewReconciler(h.store, h.deploy, h.dns, h.cert, h.notify, h.clock, "preview.example.com", 24*time.Hour, nil)
	ctx := context.Background()
	req := ApplyRequest{Project: "acme/web", Branch: "feature-x", Subdomain: "feature-x", ArtifactRef: "ref1"}

	env, err := rec.Apply(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, env.TTLExpiresAt)
	require.True(t, env.TTLExpiresAt.Equal(h.clock.now.Add(24*time.Hour)))

	h.clock.now = h.clock.now.Add(time.Hour)
	env, err = rec.Apply(ctx, req)
	require.NoError(t, err)
	require.True(t, env.TTLExpiresAt.Equal(h.clock.now.Add(24*time.Hour)), "TTL must refresh on every successful apply")
}
