// SPDX-License-Identifier: Apache-2.0

package providerapi

import "errors"

// ErrPermanent marks a provider failure that retrying cannot fix: a DNS name
// owned by someone else, a payload the provider cannot interpret, a rejected
// signature. Providers wrap it into their own sentinels so the reconciler can
// retire the work immediately instead of spending its whole retry budget on an
// outcome that will not change.
//
// Failures that are absent from this set are treated as transient and retried.
// Wrap only errors that are permanent for the caller, not merely for this
// attempt: a rate limit or a 5xx is transient even though the call failed.
var ErrPermanent = errors.New("providerapi: permanent failure")

// Permanent wraps err so errors.Is(err, ErrPermanent) reports true, preserving
// err in the chain. It returns nil for a nil err so it can wrap a call result
// directly.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }

// Unwrap returns both the wrapped error and ErrPermanent so errors.Is matches
// the original sentinel and the permanence marker alike.
func (e permanentError) Unwrap() []error { return []error{e.err, ErrPermanent} }
