// SPDX-License-Identifier: Apache-2.0

package providerapi

import "context"

// NotifyEvent is a status change to be reported to a project's contributors.
type NotifyEvent struct {
	Kind   string // "ready" | "updated" | "failed" | "expiring" | "destroyed"
	URL    string
	Detail string
}

// NotifierProvider delivers preview environment status notifications.
type NotifierProvider interface {
	// Notify delivers ev for the given project and pull request. prNumber is 0 if
	// the environment isn't associated with an open PR.
	Notify(ctx context.Context, project string, prNumber int, ev NotifyEvent) error
}
