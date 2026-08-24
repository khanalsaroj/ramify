// SPDX-License-Identifier: Apache-2.0

// Package contract holds the shared behavioral test suites every providerapi
// implementation — built-in or third-party — must pass. See docs/providers.md for
// how to run these against a real account.
package contract

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// GitProviderCase describes one ParseWebhook fixture for RunGitProviderContract.
type GitProviderCase struct {
	Name      string
	Payload   []byte
	Signature string
	WantEvent providerapi.Event
}

// RunGitProviderContract verifies the minimum behavior every providerapi.GitProvider
// implementation must satisfy: correctly signed, well-formed payloads map to the
// expected Event, and a bad signature is rejected without a false-positive parse.
func RunGitProviderContract(t *testing.T, p providerapi.GitProvider, cases []GitProviderCase, invalidSignature string) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := p.ParseWebhook(context.Background(), tc.Payload, tc.Signature)
			require.NoError(t, err)
			require.Equal(t, tc.WantEvent, got)
		})
	}

	t.Run("invalid signature is rejected", func(t *testing.T) {
		require.NotEmpty(t, cases, "at least one valid case is required to exercise the rejection path")
		_, err := p.ParseWebhook(context.Background(), cases[0].Payload, invalidSignature)
		require.Error(t, err)
	})
}
