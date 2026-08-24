// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"context"
	"sync"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// Notification is a recorded call to NotifierProvider.Notify.
type Notification struct {
	Project  string
	PRNumber int
	Event    providerapi.NotifyEvent
}

// NotifierProvider is an in-memory fake of providerapi.NotifierProvider.
type NotifierProvider struct {
	mu            sync.Mutex
	Notifications []Notification

	// NotifyErr, when set, is returned by every Notify call.
	NotifyErr error
}

var _ providerapi.NotifierProvider = (*NotifierProvider)(nil)

// NewNotifierProvider returns an empty fake NotifierProvider.
func NewNotifierProvider() *NotifierProvider {
	return &NotifierProvider{}
}

// Notify implements providerapi.NotifierProvider.
func (f *NotifierProvider) Notify(_ context.Context, project string, prNumber int, ev providerapi.NotifyEvent) error {
	if f.NotifyErr != nil {
		return f.NotifyErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Notifications = append(f.Notifications, Notification{Project: project, PRNumber: prNumber, Event: ev})
	return nil
}
