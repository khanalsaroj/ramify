// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
)

// ErrInvalidTransition is returned by UpdateEnvironment when the requested status
// is not reachable from the environment's current status. It exists so lifecycle
// bugs surface as errors instead of silently corrupting state: without it any
// whole-row update can move an environment to any status, including impossible
// ones such as destroyed -> ready.
var ErrInvalidTransition = errors.New("store: invalid status transition")

// allowedTransitions maps each status to the statuses reachable from it. A status
// is always allowed to transition to itself so that updates which only touch
// other fields (TTL refresh, pin, deploy_ref) are not rejected.
//
// The graph is deliberately permissive about re-entering deploying: a new push to
// a branch legitimately redeploys an environment that is ready, failed, sleeping,
// or already torn down.
var allowedTransitions = map[string][]string{
	StatusPending:   {StatusDeploying, StatusDestroying, StatusFailed},
	StatusDeploying: {StatusReady, StatusDegraded, StatusFailed, StatusDestroying},
	// StatusDegraded is reachable from StatusReady too: a compensating rollback
	// redeploy goes through attemptApply's normal success path (which writes
	// Ready) before the reconciler immediately re-marks it Degraded to reflect
	// that the *requested* revision, not the one now running, is what failed.
	StatusReady:      {StatusDeploying, StatusSleeping, StatusDestroying, StatusFailed, StatusDegraded},
	StatusFailed:     {StatusDeploying, StatusDestroying},
	StatusSleeping:   {StatusDeploying, StatusReady, StatusDestroying},
	StatusDestroying: {StatusDestroyed, StatusFailed},
	StatusDestroyed:  {StatusDeploying, StatusDestroying},
	// A degraded environment is still live (something is serving, just not the
	// requested revision): the only ways out are trying another deploy or
	// tearing it down, exactly like StatusFailed.
	StatusDegraded: {StatusDeploying, StatusDestroying},
}

// CanTransition reports whether an environment may move from status to next.
// Unknown statuses are rejected rather than allowed through, so a typo in a
// status constant fails loudly at the first write.
func CanTransition(from, to string) bool {
	if from == to {
		_, known := allowedTransitions[from]
		return known
	}
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// validateTransition returns a wrapped ErrInvalidTransition describing the
// rejected move, or nil when the move is legal.
func validateTransition(from, to string) error {
	if CanTransition(from, to) {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
}
