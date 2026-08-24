// SPDX-License-Identifier: Apache-2.0

// Package fakes provides deterministic in-memory implementations of every
// providerapi interface, for use in unit tests that must not touch the network.
package fakes

import (
	"context"
	"errors"
	"sync"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// GitProvider is an in-memory fake of providerapi.GitProvider. ParseWebhook results
// are queued via QueueEvent; comments are recorded for later assertion.
type GitProvider struct {
	mu       sync.Mutex
	queued   []queuedEvent
	Comments []Comment

	// ParseWebhookErr, when set, is returned by every ParseWebhook call.
	ParseWebhookErr error
	// CommentErr, when set, is returned by every CommentOnPR call.
	CommentErr error
}

type queuedEvent struct {
	sig string
	ev  providerapi.Event
}

// Comment is a recorded call to CommentOnPR.
type Comment struct {
	Project  string
	PRNumber int
	Body     string
}

var _ providerapi.GitProvider = (*GitProvider)(nil)

// NewGitProvider returns an empty fake GitProvider.
func NewGitProvider() *GitProvider {
	return &GitProvider{}
}

// QueueEvent arranges for the next ParseWebhook call with a matching signature to
// return ev.
func (f *GitProvider) QueueEvent(signature string, ev providerapi.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, queuedEvent{sig: signature, ev: ev})
}

// ErrBadSignature is returned by ParseWebhook when no matching queued event exists,
// simulating HMAC signature verification failure.
var ErrBadSignature = errors.New("fakes: bad webhook signature")

// ParseWebhook implements providerapi.GitProvider.
func (f *GitProvider) ParseWebhook(_ context.Context, _ []byte, signature string) (providerapi.Event, error) {
	if f.ParseWebhookErr != nil {
		return providerapi.Event{}, f.ParseWebhookErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, q := range f.queued {
		if q.sig == signature {
			f.queued = append(f.queued[:i], f.queued[i+1:]...)
			return q.ev, nil
		}
	}
	return providerapi.Event{}, ErrBadSignature
}

// CommentOnPR implements providerapi.GitProvider.
func (f *GitProvider) CommentOnPR(_ context.Context, project string, prNumber int, body string) error {
	if f.CommentErr != nil {
		return f.CommentErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Comments = append(f.Comments, Comment{Project: project, PRNumber: prNumber, Body: body})
	return nil
}
