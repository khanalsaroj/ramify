// SPDX-License-Identifier: Apache-2.0

package providerapi

import (
	"context"
	"errors"
)

// ErrInvalidWebhookSignature is the sentinel error a GitProvider implementation
// must return (wrapped, so errors.Is still matches) from ParseWebhook when the
// webhook signature fails verification, so callers such as the control API's HTTP
// handler can respond 401 without depending on a specific GitProvider
// implementation's error types.
var ErrInvalidWebhookSignature = errors.New("providerapi: invalid webhook signature")

// ErrUnhandledWebhookEvent is the sentinel error a GitProvider implementation must
// return (wrapped) from ParseWebhook when the payload is validly signed and
// well-formed but has no mapping to an Event (for example, an irrelevant
// pull_request action). Callers should treat it as a no-op, not a failure.
var ErrUnhandledWebhookEvent = errors.New("providerapi: unhandled webhook event")

// Event is a normalized change notification produced by a GitProvider from an
// inbound webhook payload.
type Event struct {
	Kind     string // "branch_pushed" | "pr_synchronized" | "pr_closed" | "branch_deleted"
	Project  string
	Branch   string
	PRNumber int // 0 if not a PR
	Artifact string
}

// GitProvider verifies and parses inbound git-hosting webhooks into normalized
// Events, and posts status comments back to the hosting platform.
type GitProvider interface {
	// ParseWebhook verifies signature against payload before any JSON parsing occurs
	// and returns the normalized Event on success.
	ParseWebhook(ctx context.Context, payload []byte, signature string) (Event, error)
	// CommentOnPR posts or updates a status comment on the given pull request.
	CommentOnPR(ctx context.Context, project string, prNumber int, body string) error
}
