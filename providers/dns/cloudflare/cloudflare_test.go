// SPDX-License-Identifier: Apache-2.0

package cloudflare

import (
	"context"
	"fmt"
	"sync"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
)

// fakeCFClient is an in-memory stand-in for the Cloudflare API, implementing just
// the dnsClient subset Provider uses, so unit tests never make a real API call.
type fakeCFClient struct {
	mu      sync.Mutex
	zones   map[string]string // zone name -> zone ID
	records map[string]cf.DNSRecord
	nextID  int
}

func newFakeCFClient(zoneName, zoneID string) *fakeCFClient {
	return &fakeCFClient{
		zones:   map[string]string{zoneName: zoneID},
		records: make(map[string]cf.DNSRecord),
	}
}

func (f *fakeCFClient) ZoneIDByName(zoneName string) (string, error) {
	id, ok := f.zones[zoneName]
	if !ok {
		return "", fmt.Errorf("fake: unknown zone %s", zoneName)
	}
	return id, nil
}

func (f *fakeCFClient) CreateDNSRecord(_ context.Context, _ *cf.ResourceContainer, params cf.CreateDNSRecordParams) (cf.DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	rec := cf.DNSRecord{ID: fmt.Sprintf("rec-%d", f.nextID), Type: params.Type, Name: params.Name, Content: params.Content}
	f.records[rec.ID] = rec
	return rec, nil
}

// ListDNSRecords simulates a single-page result set. cloudflare-go's
// ResultInfo.Done() only returns true once Page > TotalPages (never on the first
// Page==1 response, even when TotalPages==1 — see pagination.go), so every real
// caller of listAll makes one extra round-trip past page 1. This fake mirrors that:
// page 1 returns the filtered records, any later page returns empty.
func (f *fakeCFClient) ListDNSRecords(_ context.Context, _ *cf.ResourceContainer, params cf.ListDNSRecordsParams) ([]cf.DNSRecord, *cf.ResultInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if params.Page > 1 {
		return nil, &cf.ResultInfo{Page: params.Page, TotalPages: 1}, nil
	}

	var out []cf.DNSRecord
	for _, r := range f.records {
		if params.Name != "" && r.Name != params.Name {
			continue
		}
		if params.Type != "" && r.Type != params.Type {
			continue
		}
		out = append(out, r)
	}
	return out, &cf.ResultInfo{Page: 1, TotalPages: 1}, nil
}

func (f *fakeCFClient) UpdateDNSRecord(_ context.Context, _ *cf.ResourceContainer, params cf.UpdateDNSRecordParams) (cf.DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[params.ID]
	if !ok {
		return cf.DNSRecord{}, fmt.Errorf("fake: unknown record %s", params.ID)
	}
	rec.Content = params.Content
	f.records[params.ID] = rec
	return rec, nil
}

func (f *fakeCFClient) DeleteDNSRecord(_ context.Context, _ *cf.ResourceContainer, recordID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.records, recordID)
	return nil
}

const testZone = "preview.example.com"

func newTestProvider() (*Provider, *fakeCFClient) {
	client := newFakeCFClient(testZone, "zone-id-1")
	return newWithClient(client), client
}

func TestDNSProviderContract(t *testing.T) {
	p, _ := newTestProvider()
	contract.RunDNSProviderContract(t, p, testZone)
}

func TestEnsureRecordCreatesPairedTXT(t *testing.T) {
	p, client := newTestProvider()
	ctx := context.Background()

	rec := providerapi.DNSRecord{Zone: testZone, Name: "feature-x." + testZone, Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-abc123"}
	require.NoError(t, p.EnsureRecord(ctx, rec))

	var aCount, txtCount int
	for _, r := range client.records {
		if r.Name != rec.Name {
			continue
		}
		switch r.Type {
		case "A":
			aCount++
			require.Equal(t, rec.Value, r.Content)
		case "TXT":
			txtCount++
			require.Equal(t, rec.OwnershipTag, r.Content)
		}
	}
	require.Equal(t, 1, aCount)
	require.Equal(t, 1, txtCount)
}

func TestEnsureRecordUpdatesInPlace(t *testing.T) {
	p, client := newTestProvider()
	ctx := context.Background()

	rec := providerapi.DNSRecord{Zone: testZone, Name: "feature-x." + testZone, Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-abc123"}
	require.NoError(t, p.EnsureRecord(ctx, rec))

	rec.Value = "203.0.113.20"
	require.NoError(t, p.EnsureRecord(ctx, rec))

	var aRecords []cf.DNSRecord
	for _, r := range client.records {
		if r.Name == rec.Name && r.Type == "A" {
			aRecords = append(aRecords, r)
		}
	}
	require.Len(t, aRecords, 1, "update must not create a second A record")
	require.Equal(t, "203.0.113.20", aRecords[0].Content)
}

func TestDeleteRecordRejectsMismatchedTag(t *testing.T) {
	p, _ := newTestProvider()
	ctx := context.Background()

	rec := providerapi.DNSRecord{Zone: testZone, Name: "feature-x." + testZone, Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-abc123"}
	require.NoError(t, p.EnsureRecord(ctx, rec))

	badRec := rec
	badRec.OwnershipTag = "someone-elses-tag"
	err := p.DeleteRecord(ctx, badRec)
	require.ErrorIs(t, err, ErrOwnershipMismatch)
}

func TestDeleteRecordWithoutExistingTXTIsRejected(t *testing.T) {
	p, _ := newTestProvider()
	rec := providerapi.DNSRecord{Zone: testZone, Name: "never-created." + testZone, Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-abc123"}
	err := p.DeleteRecord(context.Background(), rec)
	require.ErrorIs(t, err, ErrOwnershipMismatch)
}

func TestListManagedRecordsOnlyReturnsRecordsWithOwnershipTXT(t *testing.T) {
	p, client := newTestProvider()
	ctx := context.Background()

	rec := providerapi.DNSRecord{Zone: testZone, Name: "feature-x." + testZone, Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-abc123"}
	require.NoError(t, p.EnsureRecord(ctx, rec))

	// A record with no TXT counterpart (not managed by Ramify) must be excluded.
	client.mu.Lock()
	client.nextID++
	client.records[fmt.Sprintf("rec-%d", client.nextID)] = cf.DNSRecord{ID: fmt.Sprintf("rec-%d", client.nextID), Type: "A", Name: "unmanaged." + testZone, Content: "203.0.113.99"}
	client.mu.Unlock()

	managed, err := p.ListManagedRecords(ctx, testZone)
	require.NoError(t, err)
	require.Len(t, managed, 1)
	require.Equal(t, rec.Name, managed[0].Name)
	require.Equal(t, rec.OwnershipTag, managed[0].OwnershipTag)
}

func TestEnsureAndDeleteTXTRecordIsSelfOwned(t *testing.T) {
	p, client := newTestProvider()
	ctx := context.Background()

	challenge := providerapi.DNSRecord{
		Zone: testZone, Name: "_acme-challenge.feature-x." + testZone, Type: "TXT",
		Value: "challenge-token-value", OwnershipTag: "unused-for-txt",
	}
	require.NoError(t, p.EnsureRecord(ctx, challenge))

	var txtCount int
	for _, r := range client.records {
		if r.Name == challenge.Name && r.Type == "TXT" {
			txtCount++
			require.Equal(t, challenge.Value, r.Content)
		}
	}
	require.Equal(t, 1, txtCount, "a TXT-type EnsureRecord must not create a second paired ownership TXT")

	// A delete with a mismatched Value must be rejected.
	wrongValue := challenge
	wrongValue.Value = "not-the-real-value"
	require.ErrorIs(t, p.DeleteRecord(ctx, wrongValue), ErrOwnershipMismatch)

	require.NoError(t, p.DeleteRecord(ctx, challenge))
	for _, r := range client.records {
		require.NotEqual(t, challenge.Name, r.Name)
	}
}

func TestZoneIDIsCached(t *testing.T) {
	p, client := newTestProvider()
	ctx := context.Background()

	rec := providerapi.DNSRecord{Zone: testZone, Name: "a." + testZone, Type: "A", Value: "1.1.1.1", OwnershipTag: "tag1"}
	require.NoError(t, p.EnsureRecord(ctx, rec))

	delete(client.zones, testZone) // if EnsureRecord re-resolves the zone, this next call fails
	rec.Value = "1.1.1.2"
	require.NoError(t, p.EnsureRecord(ctx, rec))
}
