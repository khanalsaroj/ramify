// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMarkEventDeadLetteredRemovesFromWorkSets(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ev, err := s.CreateEvent(ctx, Event{Kind: EventKindWebhookReceived, DedupeKey: "d1", Payload: "{}"})
	require.NoError(t, err)

	due, err := s.ListDueEvents(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, due, 1, "a fresh event is due")

	require.NoError(t, s.MarkEventDeadLettered(ctx, ev.ID, now, "permanent failure"))

	// A retired event must disappear from both work sets, or the worker would
	// keep picking up an event that can never succeed.
	due, err = s.ListDueEvents(ctx, now, 10)
	require.NoError(t, err)
	require.Empty(t, due)

	unprocessed, err := s.ListUnprocessedEvents(ctx)
	require.NoError(t, err)
	require.Empty(t, unprocessed)

	// ...but stay inspectable, with the cause preserved.
	dead, err := s.ListDeadLetteredEvents(ctx, 10)
	require.NoError(t, err)
	require.Len(t, dead, 1)
	require.Equal(t, ev.ID, dead[0].ID)
	require.Equal(t, "permanent failure", dead[0].LastError)
	require.NotNil(t, dead[0].DeadLetteredAt)
	require.Equal(t, 1, dead[0].Attempts)
}

func TestDeadLetteredEventCannotBeClaimedOrRetried(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ev, err := s.CreateEvent(ctx, Event{Kind: EventKindWebhookReceived, DedupeKey: "d2", Payload: "{}"})
	require.NoError(t, err)
	require.NoError(t, s.MarkEventDeadLettered(ctx, ev.ID, now, "boom"))

	claimed, err := s.ClaimEvent(ctx, ev.ID, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, claimed, "a retired event must not be leasable")

	err = s.MarkEventRetry(ctx, ev.ID, now.Add(time.Minute), "boom again")
	require.ErrorIs(t, err, ErrNotFound, "a retired event must not be reschedulable")
}

// The gauge must report the true total, not a capped page size.
func TestCountDeadLetteredEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	n, err := s.CountDeadLetteredEvents(ctx)
	require.NoError(t, err)
	require.Zero(t, n)

	for i := range 3 {
		ev, createErr := s.CreateEvent(ctx, Event{
			Kind: EventKindWebhookReceived, DedupeKey: fmt.Sprintf("c%d", i), Payload: "{}",
		})
		require.NoError(t, createErr)
		require.NoError(t, s.MarkEventDeadLettered(ctx, ev.ID, now, "boom"))
	}

	n, err = s.CountDeadLetteredEvents(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

func TestMarkEventDeadLetteredIsIdempotentGuarded(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ev, err := s.CreateEvent(ctx, Event{Kind: EventKindWebhookReceived, DedupeKey: "d3", Payload: "{}"})
	require.NoError(t, err)
	require.NoError(t, s.MarkEventDeadLettered(ctx, ev.ID, now, "first"))

	// A second attempt must not double-count the attempt or overwrite the cause.
	require.ErrorIs(t, s.MarkEventDeadLettered(ctx, ev.ID, now, "second"), ErrNotFound)

	dead, err := s.ListDeadLetteredEvents(ctx, 10)
	require.NoError(t, err)
	require.Len(t, dead, 1)
	require.Equal(t, "first", dead[0].LastError)
	require.Equal(t, 1, dead[0].Attempts)
}
