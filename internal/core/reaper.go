// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/khanalsaroj/ramify/internal/metrics"
	"github.com/khanalsaroj/ramify/internal/store"
)

// Reaper tears down environments whose TTL has expired. For the MVP this is TTL
// expiry only; idle detection and automatic sleep/wake are Phase 2.
type Reaper struct {
	store      store.Store
	reconciler *Reconciler
	clock      Clock
	logger     *slog.Logger
	metrics    *metrics.Metrics
}

// NewReaper constructs a Reaper.
func NewReaper(st store.Store, reconciler *Reconciler, clock Clock, logger *slog.Logger, metricSet ...*metrics.Metrics) *Reaper {
	if logger == nil {
		logger = slog.Default()
	}
	m := &metrics.Metrics{}
	if len(metricSet) > 0 && metricSet[0] != nil {
		m = metricSet[0]
	}
	return &Reaper{store: st, reconciler: reconciler, clock: clock, logger: logger, metrics: m}
}

// Sweep destroys every non-pinned environment whose ttl_expires_at is at or before
// the current time. A pinned environment is never swept, regardless of TTL. Sweep
// destroys as many expired environments as it can and returns a joined error for
// any that failed, so one failure doesn't block the rest.
func (r *Reaper) Sweep(ctx context.Context) error {
	r.metrics.CleanupRuns.Add(1)
	expired, err := r.store.ListExpiredEnvironments(ctx, r.clock.Now())
	if err != nil {
		r.metrics.CleanupFailures.Add(1)
		return fmt.Errorf("reaper: listing expired environments: %w", err)
	}

	var errs []error
	for _, env := range expired {
		if err := r.reconciler.Destroy(ctx, env); err != nil {
			r.logger.ErrorContext(ctx, "reaper: destroy failed", "error", err, "environment_id", env.ID)
			errs = append(errs, fmt.Errorf("environment %s: %w", env.ID, err))
		}
	}
	if len(errs) > 0 {
		r.metrics.CleanupFailures.Add(int64(len(errs)))
		return fmt.Errorf("reaper: sweep: %w", errors.Join(errs...))
	}
	return nil
}
