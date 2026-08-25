// SPDX-License-Identifier: Apache-2.0

package providerapi

import (
	"context"
	"errors"
)

// ErrRecordAlreadyAbsent indicates that the requested record is already gone.
// Destructive reconciliation should treat this as successful idempotent cleanup.
var ErrRecordAlreadyAbsent = errors.New("providerapi: dns record already absent")

// DNSRecord is a single DNS record managed by Ramify. Every record Ramify creates
// carries an OwnershipTag so it can later be safely identified for deletion, in the
// style of the external-dns project's TXT registry pattern.
type DNSRecord struct {
	Zone         string
	Name         string
	Type         string // "A" | "CNAME"
	Value        string
	OwnershipTag string // required on every record Ramify creates
}

// DNSProvider manages DNS records for preview environments.
type DNSProvider interface {
	// EnsureRecord creates or updates rec. It must be idempotent and must not
	// create a duplicate record for the same Zone/Name.
	EnsureRecord(ctx context.Context, rec DNSRecord) error
	// DeleteRecord verifies rec.OwnershipTag against the existing TXT ownership
	// marker before deleting. It returns an error, and does not silently no-op, if
	// the tag doesn't match or is absent.
	DeleteRecord(ctx context.Context, rec DNSRecord) error
	// ListManagedRecords returns every record in zone that carries a Ramify
	// OwnershipTag.
	ListManagedRecords(ctx context.Context, zone string) ([]DNSRecord, error)
}
