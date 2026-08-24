// SPDX-License-Identifier: Apache-2.0

// Package core implements the Ramify reconciler and reaper: the logic that drives
// preview environments toward their desired state using the providerapi interfaces.
package core

import "time"

// Clock abstracts time so the reconciler's retry backoff and the reaper's expiry
// checks can be tested deterministically, per the constructor-injected dependency
// rule (no package-level time.Now/time.Sleep calls in core logic).
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

type realClock struct{}

// NewRealClock returns a Clock backed by the standard library's time package.
func NewRealClock() Clock {
	return realClock{}
}

// Now implements Clock.
func (realClock) Now() time.Time { return time.Now() }

// Sleep implements Clock.
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }
