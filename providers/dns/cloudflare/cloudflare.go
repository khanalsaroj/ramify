// SPDX-License-Identifier: Apache-2.0

// Package cloudflare implements providerapi.DNSProvider against the Cloudflare DNS
// API, using a TXT ownership registry in the style of the external-dns project: for
// every A/CNAME record Ramify manages at a name, a TXT record at that same name
// carries the OwnershipTag, and DeleteRecord refuses to act unless that TXT content
// matches.
package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"sync"

	cf "github.com/cloudflare/cloudflare-go"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// ErrOwnershipMismatch is returned by DeleteRecord when no TXT record at the given
// name carries a matching OwnershipTag.
var ErrOwnershipMismatch = errors.New("cloudflare: ownership tag mismatch, refusing delete")

// ErrUnmanagedRecord indicates that a record already exists at a name but is
// not owned by the Ramify instance attempting to manage it.
var ErrUnmanagedRecord = errors.New("cloudflare: unmanaged record collision")

// dnsClient is the subset of *cloudflare.API used by Provider, narrowed to an
// interface so tests can substitute an in-memory fake instead of making real API
// calls.
type dnsClient interface {
	ZoneIDByName(zoneName string) (string, error)
	CreateDNSRecord(ctx context.Context, rc *cf.ResourceContainer, params cf.CreateDNSRecordParams) (cf.DNSRecord, error)
	ListDNSRecords(ctx context.Context, rc *cf.ResourceContainer, params cf.ListDNSRecordsParams) ([]cf.DNSRecord, *cf.ResultInfo, error)
	UpdateDNSRecord(ctx context.Context, rc *cf.ResourceContainer, params cf.UpdateDNSRecordParams) (cf.DNSRecord, error)
	DeleteDNSRecord(ctx context.Context, rc *cf.ResourceContainer, recordID string) error
}

// Provider implements providerapi.DNSProvider against Cloudflare.
type Provider struct {
	client dnsClient

	mu          sync.Mutex
	zoneIDCache map[string]string
}

var _ providerapi.DNSProvider = (*Provider)(nil)

// New constructs a Provider authenticated with a Cloudflare API token.
func New(apiToken string) (*Provider, error) {
	api, err := cf.NewWithAPIToken(apiToken)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: constructing client: %w", err)
	}
	return newWithClient(api), nil
}

func newWithClient(client dnsClient) *Provider {
	return &Provider{client: client, zoneIDCache: make(map[string]string)}
}

func (p *Provider) zoneID(zone string) (string, error) {
	p.mu.Lock()
	if id, ok := p.zoneIDCache[zone]; ok {
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()

	id, err := p.client.ZoneIDByName(zone)
	if err != nil {
		return "", fmt.Errorf("resolving zone %s: %w", zone, err)
	}

	p.mu.Lock()
	p.zoneIDCache[zone] = id
	p.mu.Unlock()
	return id, nil
}

// EnsureRecord implements providerapi.DNSProvider. For an A/CNAME record it creates
// or updates the record and a paired TXT ownership record at the same name, without
// creating a duplicate of either on repeated calls. For a TXT record — used for the
// ACME DNS-01 challenge in providers/cert/acme — there is nothing to pair it with:
// the record's own content is its identity, so no separate ownership record is
// written (see DeleteRecord).
func (p *Provider) EnsureRecord(ctx context.Context, rec providerapi.DNSRecord) error {
	zoneID, err := p.zoneID(rec.Zone)
	if err != nil {
		return fmt.Errorf("cloudflare: ensure record %s: %w", rec.Name, err)
	}
	rc := cf.ZoneIdentifier(zoneID)

	if rec.Type == "TXT" {
		// ACME and other TXT users may legitimately have multiple values at
		// one name. Never update an arbitrary TXT record.
		existing, err := p.listAll(ctx, rc, cf.ListDNSRecordsParams{Name: rec.Name, Type: "TXT"})
		if err != nil {
			return fmt.Errorf("cloudflare: list TXT records %s: %w", rec.Name, err)
		}
		for _, record := range existing {
			if record.Content == rec.Value {
				return nil
			}
		}
		if _, err := p.client.CreateDNSRecord(ctx, rc, cf.CreateDNSRecordParams{Type: "TXT", Name: rec.Name, Content: rec.Value, TTL: 60}); err != nil {
			return fmt.Errorf("cloudflare: creating TXT record %s: %w", rec.Name, err)
		}
		return nil
	}

	// A/CNAME records may only be created or updated when the matching TXT
	// ownership marker already exists or the name is otherwise empty. This
	// prevents Ramify from overwriting an operator-owned record.
	txtRecords, err := p.listAll(ctx, rc, cf.ListDNSRecordsParams{Name: rec.Name, Type: "TXT"})
	if err != nil {
		return fmt.Errorf("cloudflare: list ownership TXT %s: %w", rec.Name, err)
	}
	owned := false
	for _, record := range txtRecords {
		if record.Content == rec.OwnershipTag {
			owned = true
			break
		}
	}
	mainRecords, err := p.listAll(ctx, rc, cf.ListDNSRecordsParams{Name: rec.Name, Type: rec.Type})
	if err != nil {
		return fmt.Errorf("cloudflare: list %s records %s: %w", rec.Type, rec.Name, err)
	}
	if len(mainRecords) > 0 && !owned {
		return fmt.Errorf("cloudflare: ensure record %s: %w", rec.Name, ErrUnmanagedRecord)
	}
	if len(mainRecords) == 0 {
		if _, err := p.client.CreateDNSRecord(ctx, rc, cf.CreateDNSRecordParams{Type: rec.Type, Name: rec.Name, Content: rec.Value, TTL: 60}); err != nil {
			return fmt.Errorf("cloudflare: creating %s record %s: %w", rec.Type, rec.Name, err)
		}
	} else {
		if len(mainRecords) > 1 {
			return fmt.Errorf("cloudflare: ensure record %s: %w: multiple owned records", rec.Name, ErrUnmanagedRecord)
		}
		if _, err := p.client.UpdateDNSRecord(ctx, rc, cf.UpdateDNSRecordParams{ID: mainRecords[0].ID, Type: rec.Type, Name: rec.Name, Content: rec.Value}); err != nil {
			return fmt.Errorf("cloudflare: updating %s record %s: %w", rec.Type, rec.Name, err)
		}
	}
	if owned {
		return nil
	}
	if err := p.ensureSingleRecord(ctx, rc, rec.Name, "TXT", rec.OwnershipTag); err != nil {
		return fmt.Errorf("cloudflare: ensure ownership txt for %s: %w", rec.Name, err)
	}
	return nil
}

// ensureSingleRecord creates a record of recordType at name with the given content,
// or updates the existing one in place if it already exists, so repeated calls
// never create a duplicate.
func (p *Provider) ensureSingleRecord(ctx context.Context, rc *cf.ResourceContainer, name, recordType, content string) error {
	existing, err := p.listAll(ctx, rc, cf.ListDNSRecordsParams{Name: name, Type: recordType})
	if err != nil {
		return fmt.Errorf("listing existing %s records: %w", recordType, err)
	}

	for _, record := range existing {
		if record.Content == content {
			return nil
		}
	}
	if len(existing) == 0 {
		_, err := p.client.CreateDNSRecord(ctx, rc, cf.CreateDNSRecordParams{Type: recordType, Name: name, Content: content, TTL: 60})
		if err != nil {
			return fmt.Errorf("creating %s record: %w", recordType, err)
		}
		return nil
	}

	if recordType == "TXT" {
		_, err := p.client.CreateDNSRecord(ctx, rc, cf.CreateDNSRecordParams{Type: recordType, Name: name, Content: content, TTL: 60})
		if err != nil {
			return fmt.Errorf("creating %s record: %w", recordType, err)
		}
		return nil
	}
	_, err = p.client.UpdateDNSRecord(ctx, rc, cf.UpdateDNSRecordParams{ID: existing[0].ID, Type: recordType, Name: name, Content: content})
	if err != nil {
		return fmt.Errorf("updating %s record: %w", recordType, err)
	}
	return nil
}

// DeleteRecord implements providerapi.DNSProvider. For an A/CNAME record it reads
// the paired TXT ownership record at rec.Name and refuses to delete — returning
// ErrOwnershipMismatch rather than silently no-oping — unless its content matches
// rec.OwnershipTag. For a TXT record (see EnsureRecord), the record's own content is
// its identity: deletion is authorized by an existing TXT record at rec.Name whose
// content matches rec.Value.
func (p *Provider) DeleteRecord(ctx context.Context, rec providerapi.DNSRecord) error {
	zoneID, err := p.zoneID(rec.Zone)
	if err != nil {
		return fmt.Errorf("cloudflare: delete record %s: %w", rec.Name, err)
	}
	rc := cf.ZoneIdentifier(zoneID)

	ownerContent := rec.OwnershipTag
	if rec.Type == "TXT" {
		ownerContent = rec.Value
	}

	txtRecords, err := p.listAll(ctx, rc, cf.ListDNSRecordsParams{Name: rec.Name, Type: "TXT"})
	if err != nil {
		return fmt.Errorf("cloudflare: delete record %s: listing ownership txt: %w", rec.Name, err)
	}

	var owned *cf.DNSRecord
	for i := range txtRecords {
		if txtRecords[i].Content == ownerContent {
			owned = &txtRecords[i]
			break
		}
	}
	if owned == nil {
		return fmt.Errorf("cloudflare: delete record %s: %w", rec.Name, ErrOwnershipMismatch)
	}

	if rec.Type != "TXT" {
		mainRecords, err := p.listAll(ctx, rc, cf.ListDNSRecordsParams{Name: rec.Name, Type: rec.Type})
		if err != nil {
			return fmt.Errorf("cloudflare: delete record %s: listing record: %w", rec.Name, err)
		}
		for _, r := range mainRecords {
			if r.Content != rec.Value {
				continue
			}
			if err := p.client.DeleteDNSRecord(ctx, rc, r.ID); err != nil {
				return fmt.Errorf("cloudflare: delete record %s: %w", rec.Name, err)
			}
		}
	}

	if err := p.client.DeleteDNSRecord(ctx, rc, owned.ID); err != nil {
		return fmt.Errorf("cloudflare: delete record %s: deleting txt record: %w", rec.Name, err)
	}
	return nil
}

// ListManagedRecords implements providerapi.DNSProvider, returning every A/CNAME
// record in zone that has a paired Ramify-owned TXT ownership record at the same
// name.
func (p *Provider) ListManagedRecords(ctx context.Context, zone string) ([]providerapi.DNSRecord, error) {
	zoneID, err := p.zoneID(zone)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: list managed records: %w", err)
	}
	rc := cf.ZoneIdentifier(zoneID)

	all, err := p.listAll(ctx, rc, cf.ListDNSRecordsParams{})
	if err != nil {
		return nil, fmt.Errorf("cloudflare: list managed records: %w", err)
	}

	txtByName := make(map[string]string)
	for _, r := range all {
		if r.Type == "TXT" {
			txtByName[r.Name] = r.Content
		}
	}

	var out []providerapi.DNSRecord
	for _, r := range all {
		if r.Type == "TXT" {
			continue
		}
		tag, ok := txtByName[r.Name]
		if !ok {
			continue
		}
		out = append(out, providerapi.DNSRecord{Zone: zone, Name: r.Name, Type: r.Type, Value: r.Content, OwnershipTag: tag})
	}
	return out, nil
}

// listAll fetches every page of a ListDNSRecords query.
func (p *Provider) listAll(ctx context.Context, rc *cf.ResourceContainer, params cf.ListDNSRecordsParams) ([]cf.DNSRecord, error) {
	var out []cf.DNSRecord
	for {
		page, info, err := p.client.ListDNSRecords(ctx, rc, params)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if info == nil || info.Done() {
			return out, nil
		}
		params.ResultInfo = info.Next()
	}
}
