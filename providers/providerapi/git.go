// SPDX-License-Identifier: Apache-2.0

package providerapi

import "context"

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
