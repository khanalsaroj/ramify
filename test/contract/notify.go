// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// RunNotifierProviderContract verifies the one behavior every
// providerapi.NotifierProvider implementation must satisfy: a well-formed
// NotifyEvent delivered for a real pull/merge request does not error. It
// deliberately does not assert delivery-channel specifics (which NotifyEvent.Kind
// values have default templates, whether prNumber 0 is a no-op, and so on) —
// those are choices a given implementation makes, not part of the interface's
// contract.
func RunNotifierProviderContract(t *testing.T, p providerapi.NotifierProvider, project string) {
	t.Helper()
	ctx := context.Background()

	t.Run("delivers a well-formed event", func(t *testing.T) {
		err := p.Notify(ctx, project, 1, providerapi.NotifyEvent{
			Kind: "ready",
			URL:  "https://contract-branch.preview.example.com",
		})
		require.NoError(t, err)
	})
}
