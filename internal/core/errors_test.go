// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/providers/dns/cloudflare"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

func TestIsTerminal(t *testing.T) {
	base := errors.New("boom")

	require.False(t, IsTerminal(nil))
	require.False(t, IsTerminal(base), "an unclassified error stays retryable")
	require.True(t, IsTerminal(Terminal(base)))
	require.True(t, IsTerminal(fmt.Errorf("wrapped: %w", Terminal(base))),
		"terminality must survive wrapping by callers")

	// Provider sentinels marked permanent must classify without core importing
	// the concrete provider.
	require.True(t, IsTerminal(cloudflare.ErrUnmanagedRecord))
	require.True(t, IsTerminal(fmt.Errorf("ensure record: %w", cloudflare.ErrOwnershipMismatch)))
	require.True(t, IsTerminal(providerapi.Permanent(base)))

	// A transient provider error must not be mistaken for permanent.
	require.False(t, IsTerminal(providerapi.ErrRecordAlreadyAbsent))
}

// Permanent must preserve the original error in the chain so callers can still
// match the specific sentinel, not just the permanence marker.
func TestPermanentPreservesSentinel(t *testing.T) {
	require.ErrorIs(t, cloudflare.ErrUnmanagedRecord, providerapi.ErrPermanent)
	require.ErrorIs(t, fmt.Errorf("ctx: %w", cloudflare.ErrUnmanagedRecord), cloudflare.ErrUnmanagedRecord)
	require.Nil(t, providerapi.Permanent(nil))
	require.Nil(t, Terminal(nil))
}

func TestFullJitterStaysWithinBounds(t *testing.T) {
	require.Zero(t, fullJitter(0))
	require.Zero(t, fullJitter(-time.Second))

	for _, base := range []time.Duration{time.Second, 30 * time.Second, 5 * time.Minute} {
		spread := map[time.Duration]bool{}
		for range 200 {
			got := fullJitter(base)
			require.GreaterOrEqual(t, got, base/2, "jitter must not collapse the backoff curve")
			require.LessOrEqual(t, got, base, "jitter must not extend past the computed delay")
			spread[got] = true
		}
		require.Greater(t, len(spread), 1, "jitter must actually vary, or retries stay synchronized")
	}
}

// A permanent failure must be retired immediately rather than consuming the
// whole retry budget.
func TestScheduleRetryDeadLettersTerminalFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ev, err := h.store.CreateEvent(ctx, store.Event{Kind: store.EventKindWebhookReceived, DedupeKey: "t1", Payload: "{}"})
	require.NoError(t, err)

	h.rec.scheduleRetry(ctx, ev, Terminal(errors.New("unmanaged record")))

	due, err := h.store.ListDueEvents(ctx, h.clock.now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, due, "a permanently failed event must not be retried")

	dead, err := h.store.ListDeadLetteredEvents(ctx, 10)
	require.NoError(t, err)
	require.Len(t, dead, 1)
	require.Equal(t, ev.ID, dead[0].ID)
}

// A transient failure must be rescheduled, not retired.
func TestScheduleRetryReschedulesTransientFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ev, err := h.store.CreateEvent(ctx, store.Event{Kind: store.EventKindWebhookReceived, DedupeKey: "t2", Payload: "{}"})
	require.NoError(t, err)

	h.rec.scheduleRetry(ctx, ev, errors.New("provider timeout"))

	dead, err := h.store.ListDeadLetteredEvents(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, dead)

	due, err := h.store.ListDueEvents(ctx, h.clock.now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, 1, due[0].Attempts)
	require.NotNil(t, due[0].NextAttemptAt)
}

// An event that keeps failing must eventually be retired instead of retrying
// forever.
func TestScheduleRetryDeadLettersAfterAttemptBudget(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ev, err := h.store.CreateEvent(ctx, store.Event{Kind: store.EventKindWebhookReceived, DedupeKey: "t3", Payload: "{}"})
	require.NoError(t, err)

	transient := errors.New("provider timeout")
	for attempt := range maxEventAttempts {
		due, listErr := h.store.ListDueEvents(ctx, h.clock.now.Add(24*time.Hour), 10)
		require.NoError(t, listErr)
		if attempt < maxEventAttempts-1 {
			require.Len(t, due, 1, "event must still be pending at attempt %d", attempt)
		}
		if len(due) == 0 {
			break
		}
		h.rec.scheduleRetry(ctx, due[0], transient)
	}

	dead, err := h.store.ListDeadLetteredEvents(ctx, 10)
	require.NoError(t, err)
	require.Len(t, dead, 1, "an event must not retry past its attempt budget")
	require.Equal(t, ev.ID, dead[0].ID)
	require.Equal(t, maxEventAttempts, dead[0].Attempts)
}

// A malformed payload previously left the event pending forever: it was never
// marked processed and never rescheduled. It must now be retired.
func TestReplayRetiresUnparseablePayload(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ev, err := h.store.CreateEvent(ctx, store.Event{
		Kind:    EventKindApplyRequested,
		Payload: "{not valid json",
	})
	require.NoError(t, err)

	h.rec.ProcessEvent(ctx, ev)

	due, err := h.store.ListDueEvents(ctx, h.clock.now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, due, "an unparseable event must not stay pending forever")

	dead, err := h.store.ListDeadLetteredEvents(ctx, 10)
	require.NoError(t, err)
	require.Len(t, dead, 1)
	require.Equal(t, ev.ID, dead[0].ID)
}

func TestReplayRetiresUnknownEventKind(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ev, err := h.store.CreateEvent(ctx, store.Event{Kind: "not_a_real_kind", Payload: "{}"})
	require.NoError(t, err)

	h.rec.ProcessEvent(ctx, ev)

	dead, err := h.store.ListDeadLetteredEvents(ctx, 10)
	require.NoError(t, err)
	require.Len(t, dead, 1)
	require.Equal(t, ev.ID, dead[0].ID)
}
