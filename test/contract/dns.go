// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// RunDNSProviderContract verifies the minimum behavior every
// providerapi.DNSProvider implementation must satisfy: EnsureRecord creates and is
// idempotent (no duplicate record), ListManagedRecords reflects it, and
// DeleteRecord rejects a request whose OwnershipTag doesn't match the stored
// record.
func RunDNSProviderContract(t *testing.T, p providerapi.DNSProvider, zone string) {
	t.Helper()
	ctx := context.Background()

	rec := providerapi.DNSRecord{
		Zone:         zone,
		Name:         "contract." + zone,
		Type:         "A",
		Value:        "203.0.113.10",
		OwnershipTag: "contract-owner-tag",
	}

	require.NoError(t, p.EnsureRecord(ctx, rec))

	t.Run("ensure record is idempotent", func(t *testing.T) {
		require.NoError(t, p.EnsureRecord(ctx, rec))

		records, err := p.ListManagedRecords(ctx, zone)
		require.NoError(t, err)
		count := 0
		for _, r := range records {
			if r.Name == rec.Name {
				count++
			}
		}
		require.Equal(t, 1, count, "EnsureRecord called twice must not create a duplicate record")
	})

	t.Run("delete rejects mismatched ownership tag", func(t *testing.T) {
		unowned := rec
		unowned.OwnershipTag = "someone-elses-tag"
		require.Error(t, p.DeleteRecord(ctx, unowned))

		records, err := p.ListManagedRecords(ctx, zone)
		require.NoError(t, err)
		found := false
		for _, r := range records {
			if r.Name == rec.Name {
				found = true
			}
		}
		require.True(t, found, "a rejected delete must not remove the record")
	})

	t.Run("delete with correct ownership tag succeeds", func(t *testing.T) {
		require.NoError(t, p.DeleteRecord(ctx, rec))

		records, err := p.ListManagedRecords(ctx, zone)
		require.NoError(t, err)
		for _, r := range records {
			require.NotEqual(t, rec.Name, r.Name)
		}
	})
}
