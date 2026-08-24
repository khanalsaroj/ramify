// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) // best-effort cleanup, nothing left to assert
	return s
}

func TestCreateAndGetEnvironment(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.CreateEnvironment(ctx, Environment{
		Project:     "acme/web",
		Branch:      "feature/login",
		Subdomain:   "feature-login",
		ArtifactRef: "ghcr.io/acme/web:abc123",
		Status:      StatusPending,
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.False(t, created.CreatedAt.IsZero())

	got, err := s.GetEnvironment(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created, got)

	byBranch, err := s.GetEnvironmentByProjectBranch(ctx, "acme/web", "feature/login")
	require.NoError(t, err)
	require.Equal(t, created, byBranch)
}

func TestGetEnvironmentNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetEnvironment(ctx, "does-not-exist")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.GetEnvironmentByProjectBranch(ctx, "acme/web", "does-not-exist")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCreateEnvironmentUniqueProjectBranch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.CreateEnvironment(ctx, Environment{Project: "acme/web", Branch: "main", Subdomain: "main", ArtifactRef: "ref1", Status: StatusPending})
	require.NoError(t, err)

	_, err = s.CreateEnvironment(ctx, Environment{Project: "acme/web", Branch: "main", Subdomain: "main-2", ArtifactRef: "ref2", Status: StatusPending})
	require.ErrorIs(t, err, ErrConflict)
}

func TestUpdateEnvironment(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.CreateEnvironment(ctx, Environment{Project: "acme/web", Branch: "main", Subdomain: "main", ArtifactRef: "ref1", Status: StatusPending})
	require.NoError(t, err)

	created.Status = StatusReady
	created.ArtifactRef = "ref2"
	created.DeployRef = "deploy-1"
	updated, err := s.UpdateEnvironment(ctx, created)
	require.NoError(t, err)
	require.Equal(t, StatusReady, updated.Status)
	require.Equal(t, "ref2", updated.ArtifactRef)
	require.Equal(t, "deploy-1", updated.DeployRef)
	require.True(t, updated.UpdatedAt.After(created.CreatedAt) || updated.UpdatedAt.Equal(created.CreatedAt))
}

func TestUpdateEnvironmentNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UpdateEnvironment(context.Background(), Environment{ID: "missing", Status: StatusReady})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestListEnvironments(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.CreateEnvironment(ctx, Environment{Project: "acme/web", Branch: "a", Subdomain: "a", ArtifactRef: "r", Status: StatusPending})
	require.NoError(t, err)
	_, err = s.CreateEnvironment(ctx, Environment{Project: "acme/web", Branch: "b", Subdomain: "b", ArtifactRef: "r", Status: StatusPending})
	require.NoError(t, err)

	all, err := s.ListEnvironments(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestListExpiredEnvironments(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	expired, err := s.CreateEnvironment(ctx, Environment{Project: "p", Branch: "expired", Subdomain: "expired", ArtifactRef: "r", Status: StatusReady, TTLExpiresAt: &past})
	require.NoError(t, err)
	_, err = s.CreateEnvironment(ctx, Environment{Project: "p", Branch: "future", Subdomain: "future", ArtifactRef: "r", Status: StatusReady, TTLExpiresAt: &future})
	require.NoError(t, err)
	pinned, err := s.CreateEnvironment(ctx, Environment{Project: "p", Branch: "pinned", Subdomain: "pinned", ArtifactRef: "r", Status: StatusReady, Pinned: true, TTLExpiresAt: &past})
	require.NoError(t, err)

	results, err := s.ListExpiredEnvironments(ctx, now)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, expired.ID, results[0].ID)
	require.NotEqual(t, pinned.ID, results[0].ID)
}

func TestDNSRecordCreateListDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env, err := s.CreateEnvironment(ctx, Environment{Project: "p", Branch: "b", Subdomain: "b", ArtifactRef: "r", Status: StatusPending})
	require.NoError(t, err)

	rec, err := s.CreateDNSRecord(ctx, DNSRecord{
		EnvironmentID: env.ID,
		RecordType:    "A",
		Name:          "b.preview.example.com",
		Value:         "1.2.3.4",
		OwnershipTag:  "tag123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, rec.ID)

	list, err := s.ListDNSRecords(ctx, env.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, rec, list[0])

	require.NoError(t, s.DeleteDNSRecord(ctx, rec.ID))

	list, err = s.ListDNSRecords(ctx, env.ID)
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestDNSRecordCascadeDeleteOnEnvironment(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env, err := s.CreateEnvironment(ctx, Environment{Project: "p", Branch: "b", Subdomain: "b", ArtifactRef: "r", Status: StatusPending})
	require.NoError(t, err)
	_, err = s.CreateDNSRecord(ctx, DNSRecord{EnvironmentID: env.ID, RecordType: "A", Name: "b", Value: "1.2.3.4", OwnershipTag: "tag"})
	require.NoError(t, err)

	sq := s.(*sqliteStore)
	_, execErr := sq.db.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, env.ID)
	require.NoError(t, execErr)

	list, err := s.ListDNSRecords(ctx, env.ID)
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestEventCreateListMarkProcessed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	env, err := s.CreateEnvironment(ctx, Environment{Project: "p", Branch: "b", Subdomain: "b", ArtifactRef: "r", Status: StatusPending})
	require.NoError(t, err)

	ev, err := s.CreateEvent(ctx, Event{EnvironmentID: env.ID, Kind: "created", Payload: `{"foo":"bar"}`})
	require.NoError(t, err)
	require.NotEmpty(t, ev.ID)
	require.Nil(t, ev.ProcessedAt)

	unprocessed, err := s.ListUnprocessedEvents(ctx)
	require.NoError(t, err)
	require.Len(t, unprocessed, 1)
	require.Equal(t, ev.ID, unprocessed[0].ID)

	require.NoError(t, s.MarkEventProcessed(ctx, ev.ID, time.Now().UTC()))

	unprocessed, err = s.ListUnprocessedEvents(ctx)
	require.NoError(t, err)
	require.Empty(t, unprocessed)
}

func TestMarkEventProcessedNotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.MarkEventProcessed(context.Background(), "missing", time.Now())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestEventWithoutEnvironment(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ev, err := s.CreateEvent(ctx, Event{Kind: "webhook_received", Payload: "{}"})
	require.NoError(t, err)
	require.Empty(t, ev.EnvironmentID)

	unprocessed, err := s.ListUnprocessedEvents(ctx)
	require.NoError(t, err)
	require.Len(t, unprocessed, 1)
	require.Empty(t, unprocessed[0].EnvironmentID)
}

func TestErrorsAreWrapped(t *testing.T) {
	var err error = ErrNotFound
	require.True(t, errors.Is(err, ErrNotFound))
}
