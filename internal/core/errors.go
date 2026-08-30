// SPDX-License-Identifier: Apache-2.0

package core

import (
	"errors"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// ErrInvalidEnvironmentAction is returned by Sleep/Wake when the environment's
// current status makes the requested action impossible — most importantly, it is
// what stops a stale DeployRef on a failed or destroyed environment from ever
// reaching a provider call: Wake requires StatusSleeping and Sleep requires
// StatusReady, so an environment in any other status is rejected before doSleep/
// doWake touches the deploy provider.
var ErrInvalidEnvironmentAction = errors.New("core: action not valid for environment's current status")

// terminalError marks a failure that retrying cannot fix. Retrying a permanent
// failure — a malformed payload, a DNS name owned by someone else — burns the
// attempt budget and delays every other event behind it, so the reconciler
// retires these immediately instead of backing off.
type terminalError struct{ err error }

func (e terminalError) Error() string { return e.err.Error() }
func (e terminalError) Unwrap() error { return e.err }

// Terminal marks err as permanently failed. It returns nil for a nil err so it
// can wrap a call result directly.
func Terminal(err error) error {
	if err == nil {
		return nil
	}
	return terminalError{err: err}
}

// IsTerminal reports whether err is a permanent failure that must not be
// retried, either because Terminal wrapped it here or because a provider marked
// it with providerapi.ErrPermanent. Everything else is assumed transient, which
// keeps an unclassified error retryable rather than silently dropped.
func IsTerminal(err error) bool {
	if err == nil {
		return false
	}
	var terminal terminalError
	return errors.As(err, &terminal) || errors.Is(err, providerapi.ErrPermanent)
}
