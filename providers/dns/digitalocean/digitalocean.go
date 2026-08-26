// SPDX-License-Identifier: Apache-2.0

// Package digitalocean implements providerapi.DNSProvider against the DigitalOcean
// DNS API, using a TXT ownership registry in the style of the external-dns
// project: for every A/CNAME record Ramify manages at a name, a TXT record at that
// same name carries the OwnershipTag, and DeleteRecord refuses to act unless that
// TXT content matches.
//
// DigitalOcean scopes records to a domain rather than a zone, and stores names
// relative to it, so this package converts between the relative names the API uses
// and the fully qualified names providerapi.DNSRecord carries.
package digitalocean

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitalocean/godo"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// ErrOwnershipMismatch is returned by DeleteRecord when no TXT record at the given
// name carries a matching OwnershipTag. It is permanent: the record belongs to
// someone else, and retrying will never make deleting it safe.
var ErrOwnershipMismatch = providerapi.Permanent(errors.New("digitalocean: ownership tag mismatch"))

// ErrUnmanagedRecord indicates that a record already exists at a name but is not
// owned by the Ramify instance attempting to manage it. Permanent for the same
// reason as ErrOwnershipMismatch: only an operator can resolve the collision.
var ErrUnmanagedRecord = providerapi.Permanent(errors.New("digitalocean: unmanaged record collision"))

// recordsPerPage is the page size used for every listing. DigitalOcean caps it at
// 200.
const recordsPerPage = 200

// Provider implements providerapi.DNSProvider against one DigitalOcean domain.
type Provider struct {
	client *godo.Client
	domain string
}

var _ providerapi.DNSProvider = (*Provider)(nil)

// New constructs a Provider authenticated with a DigitalOcean personal access
// token, managing records under the given domain.
func New(token, domain string) *Provider {
	return &Provider{client: godo.NewFromToken(token), domain: strings.TrimSuffix(domain, ".")}
}
func (p *Provider) relative(name string) string {
	name = strings.TrimSuffix(name, ".")
	if name == p.domain {
		return "@"
	}
	return strings.TrimSuffix(strings.TrimSuffix(name, "."+p.domain), ".")
}
func (p *Provider) full(name string) string {
	if name == "@" || name == "" {
		return p.domain
	}
	if strings.HasSuffix(name, "."+p.domain) {
		return name
	}
	return name + "." + p.domain
}
func (p *Provider) records(ctx context.Context, typ, name string) ([]godo.DomainRecord, error) {
	var out []godo.DomainRecord
	for page := 1; ; page++ {
		rs, resp, err := p.client.Domains.Records(ctx, p.domain, &godo.ListOptions{Page: page, PerPage: recordsPerPage})
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if r.Type == typ && p.full(r.Name) == p.full(name) {
				out = append(out, r)
			}
		}
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return out, nil
		}
	}
}
func (p *Provider) create(ctx context.Context, typ, name, data string) error {
	_, _, err := p.client.Domains.CreateRecord(ctx, p.domain, &godo.DomainRecordEditRequest{Type: typ, Name: p.relative(name), Data: data, TTL: 60})
	return err
}

// EnsureRecord implements providerapi.DNSProvider. For an A/CNAME record it
// creates or updates the record and a paired TXT ownership record at the same
// name, refusing to touch a name that already holds a record Ramify does not own.
// For a TXT record — used for the ACME DNS-01 challenge — the value is added
// alongside any existing values, since one name legitimately carries several.
func (p *Provider) EnsureRecord(ctx context.Context, rec providerapi.DNSRecord) error {
	if rec.Type != "A" && rec.Type != "CNAME" && rec.Type != "TXT" {
		return fmt.Errorf("digitalocean: unsupported record type %s", rec.Type)
	}
	existing, err := p.records(ctx, rec.Type, rec.Name)
	if err != nil {
		return err
	}
	for _, r := range existing {
		if r.Data == rec.Value {
			return nil
		}
	}
	if rec.Type == "TXT" {
		return p.create(ctx, "TXT", rec.Name, rec.Value)
	}
	owners, err := p.records(ctx, "TXT", rec.Name)
	if err != nil {
		return err
	}
	owned := false
	for _, r := range owners {
		if r.Data == rec.OwnershipTag {
			owned = true
		}
	}
	if len(existing) > 0 && !owned {
		return ErrUnmanagedRecord
	}
	if len(existing) == 0 {
		if err := p.create(ctx, rec.Type, rec.Name, rec.Value); err != nil {
			return err
		}
	} else {
		_, _, err := p.client.Domains.EditRecord(ctx, p.domain, existing[0].ID, &godo.DomainRecordEditRequest{Type: rec.Type, Name: p.relative(rec.Name), Data: rec.Value, TTL: existing[0].TTL})
		if err != nil {
			return err
		}
	}
	if !owned {
		return p.create(ctx, "TXT", rec.Name, rec.OwnershipTag)
	}
	return nil
}

// DeleteRecord implements providerapi.DNSProvider, refusing to delete anything not
// vouched for by a matching TXT ownership record at the same name.
func (p *Provider) DeleteRecord(ctx context.Context, rec providerapi.DNSRecord) error {
	owners, err := p.records(ctx, "TXT", rec.Name)
	if err != nil {
		return err
	}
	owner := rec.OwnershipTag
	if rec.Type == "TXT" {
		owner = rec.Value
	}
	var ownerRecord *godo.DomainRecord
	for i := range owners {
		if owners[i].Data == owner {
			ownerRecord = &owners[i]
			break
		}
	}
	if ownerRecord == nil {
		return ErrOwnershipMismatch
	}
	if rec.Type != "TXT" {
		values, err := p.records(ctx, rec.Type, rec.Name)
		if err != nil {
			return err
		}
		for _, r := range values {
			if r.Data == rec.Value {
				if _, err := p.client.Domains.DeleteRecord(ctx, p.domain, r.ID); err != nil {
					return err
				}
			}
		}
	}
	if _, err := p.client.Domains.DeleteRecord(ctx, p.domain, ownerRecord.ID); err != nil {
		return err
	}
	return nil
}

// ListManagedRecords implements providerapi.DNSProvider, returning every non-TXT
// record under the domain that has a TXT ownership record at the same name.
func (p *Provider) ListManagedRecords(ctx context.Context, zone string) ([]providerapi.DNSRecord, error) {
	var all []godo.DomainRecord
	for page := 1; ; page++ {
		rs, resp, err := p.client.Domains.Records(ctx, p.domain, &godo.ListOptions{Page: page, PerPage: recordsPerPage})
		if err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
	}
	owners := map[string]string{}
	for _, r := range all {
		if r.Type == "TXT" {
			owners[p.full(r.Name)] = r.Data
		}
	}
	var out []providerapi.DNSRecord
	for _, r := range all {
		if r.Type == "TXT" {
			continue
		}
		if tag, ok := owners[p.full(r.Name)]; ok {
			out = append(out, providerapi.DNSRecord{Zone: zone, Name: p.full(r.Name), Type: r.Type, Value: r.Data, OwnershipTag: tag})
		}
	}
	return out, nil
}
