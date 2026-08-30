// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"context"
	"fmt"
	"sync"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// DNSProvider is an in-memory fake of providerapi.DNSProvider.
type DNSProvider struct {
	mu      sync.Mutex
	records map[string]providerapi.DNSRecord // keyed by Zone+"/"+Name

	// DeleteErr, when set, is returned by every DeleteRecord call instead of
	// succeeding, so tests can exercise compensating-cleanup ordering when one
	// teardown step fails.
	DeleteErr error
}

var _ providerapi.DNSProvider = (*DNSProvider)(nil)

// NewDNSProvider returns an empty fake DNSProvider.
func NewDNSProvider() *DNSProvider {
	return &DNSProvider{records: make(map[string]providerapi.DNSRecord)}
}

func dnsKey(zone, name string) string {
	return zone + "/" + name
}

// EnsureRecord implements providerapi.DNSProvider.
func (f *DNSProvider) EnsureRecord(_ context.Context, rec providerapi.DNSRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[dnsKey(rec.Zone, rec.Name)] = rec
	return nil
}

// DeleteRecord implements providerapi.DNSProvider. It refuses to delete when the
// stored record's OwnershipTag doesn't match rec.OwnershipTag.
func (f *DNSProvider) DeleteRecord(_ context.Context, rec providerapi.DNSRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DeleteErr != nil {
		return f.DeleteErr
	}
	key := dnsKey(rec.Zone, rec.Name)
	existing, ok := f.records[key]
	if !ok {
		return fmt.Errorf("fakes: no such dns record %s", key)
	}
	if existing.OwnershipTag != rec.OwnershipTag {
		return fmt.Errorf("fakes: ownership tag mismatch for %s: refusing delete", key)
	}
	delete(f.records, key)
	return nil
}

// ListManagedRecords implements providerapi.DNSProvider.
func (f *DNSProvider) ListManagedRecords(_ context.Context, zone string) ([]providerapi.DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []providerapi.DNSRecord
	for _, rec := range f.records {
		if rec.Zone == zone {
			out = append(out, rec)
		}
	}
	return out, nil
}
