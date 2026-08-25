// SPDX-License-Identifier: Apache-2.0

// Package metrics contains the small, dependency-free operational metrics
// surface exposed by ramifyd. The output is Prometheus text format so operators
// can scrape it without adding another runtime dependency.
package metrics

import (
	"fmt"
	"io"
	"sync/atomic"
)

// Metrics is safe for concurrent use by HTTP handlers, workers, and the reaper.
type Metrics struct {
	WebhookReceived        atomic.Int64
	WebhookRejected        atomic.Int64
	WebhookDuplicates      atomic.Int64
	Reconciliations        atomic.Int64
	ReconciliationFailures atomic.Int64
	Retries                atomic.Int64
	CleanupRuns            atomic.Int64
	CleanupFailures        atomic.Int64
	InboxPending           atomic.Int64
	// DeadLettered counts events retired without succeeding. A non-zero value
	// means work was dropped and needs an operator, so it is the counter worth
	// alerting on.
	DeadLettered atomic.Int64
}

// WritePrometheus writes the current counters in Prometheus exposition format.
func (m *Metrics) WritePrometheus(w io.Writer) error {
	values := []struct {
		name  string
		help  string
		value int64
	}{
		{"ramify_webhook_received_total", "Webhook deliveries accepted for processing", m.WebhookReceived.Load()},
		{"ramify_webhook_rejected_total", "Webhook deliveries rejected", m.WebhookRejected.Load()},
		{"ramify_webhook_duplicates_total", "Duplicate webhook deliveries", m.WebhookDuplicates.Load()},
		{"ramify_reconciliations_total", "Reconciliation attempts", m.Reconciliations.Load()},
		{"ramify_reconciliation_failures_total", "Failed reconciliations", m.ReconciliationFailures.Load()},
		{"ramify_retries_total", "Retry attempts", m.Retries.Load()},
		{"ramify_cleanup_runs_total", "Reaper cleanup runs", m.CleanupRuns.Load()},
		{"ramify_cleanup_failures_total", "Failed reaper cleanup runs", m.CleanupFailures.Load()},
	}
	for _, value := range values {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", value.name, value.help, value.name, value.name, value.value); err != nil {
			return err
		}
	}
	gauges := []struct {
		name  string
		help  string
		value int64
	}{
		{"ramify_inbox_pending", "Durable webhook inbox events pending processing", m.InboxPending.Load()},
		{"ramify_events_dead_lettered", "Events retired without succeeding; requires operator attention", m.DeadLettered.Load()},
	}
	for _, gauge := range gauges {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", gauge.name, gauge.help, gauge.name, gauge.name, gauge.value); err != nil {
			return err
		}
	}
	return nil
}
