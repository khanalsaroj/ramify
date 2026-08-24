// SPDX-License-Identifier: Apache-2.0

package dnsfile

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
)

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.zone")
	require.NoError(t, os.WriteFile(path, []byte(Header("preview.example.com")), 0o600))
	return New(path, "preview.example.com")
}

func TestDNSProviderContract(t *testing.T) {
	contract.RunDNSProviderContract(t, newTestProvider(t), "preview.example.com")
}

func TestEnsureRecordWritesPairedTXT(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	rec := providerapi.DNSRecord{Zone: "preview.example.com", Name: "feature-x.preview.example.com", Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-abc123"}
	require.NoError(t, p.EnsureRecord(ctx, rec))

	managed, err := p.ListManagedRecords(ctx, "preview.example.com")
	require.NoError(t, err)
	require.Len(t, managed, 1)
	require.Equal(t, "feature-x.preview.example.com", managed[0].Name)
	require.Equal(t, "203.0.113.10", managed[0].Value)
	require.Equal(t, "ramify-abc123", managed[0].OwnershipTag)
}

func TestEnsureRecordTXTTypeHasNoPairedOwnershipRecord(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	challenge := providerapi.DNSRecord{Zone: "preview.example.com", Name: "_acme-challenge.feature-x.preview.example.com", Type: "TXT", Value: "challenge-value"}
	require.NoError(t, p.EnsureRecord(ctx, challenge))

	records, err := p.readManaged()
	require.NoError(t, err)
	require.Len(t, records, 1, "a TXT-type EnsureRecord must not create a second paired TXT")

	require.NoError(t, p.DeleteRecord(ctx, challenge))
	records, err = p.readManaged()
	require.NoError(t, err)
	require.Empty(t, records)
}

func TestDeleteRejectsMismatchedOwnershipTag(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()

	rec := providerapi.DNSRecord{Zone: "preview.example.com", Name: "feature-x.preview.example.com", Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-abc123"}
	require.NoError(t, p.EnsureRecord(ctx, rec))

	bad := rec
	bad.OwnershipTag = "not-the-real-tag"
	require.ErrorIs(t, p.DeleteRecord(ctx, bad), ErrOwnershipMismatch)
}

func TestSerialIncreasesOnEveryWrite(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()
	rec := providerapi.DNSRecord{Zone: "preview.example.com", Name: "a.preview.example.com", Type: "A", Value: "203.0.113.10", OwnershipTag: "tag1"}

	require.NoError(t, p.EnsureRecord(ctx, rec))
	first := p.serial

	rec.Value = "203.0.113.20"
	require.NoError(t, p.EnsureRecord(ctx, rec))
	require.Greater(t, p.serial, first)
}
