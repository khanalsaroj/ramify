// SPDX-License-Identifier: Apache-2.0

// Package googlecloud implements providerapi.DNSProvider against Google Cloud DNS,
// using a TXT ownership registry in the style of the external-dns project: for
// every A/CNAME record Ramify manages at a name, a TXT record at that same name
// carries the OwnershipTag, and DeleteRecord refuses to act unless that TXT
// content matches.
//
// Credentials come from Application Default Credentials, so an operator configures
// this the same way they configure gcloud.
package googlecloud

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"google.golang.org/api/dns/v1"
)

// ErrOwnershipMismatch is returned by DeleteRecord when no TXT record at the given
// name carries a matching OwnershipTag. It is permanent: the record belongs to
// someone else, and retrying will never make deleting it safe.
var ErrOwnershipMismatch = providerapi.Permanent(errors.New("google cloud dns: ownership tag mismatch"))

// ErrUnmanagedRecord indicates that a record already exists at a name but is not
// owned by the Ramify instance attempting to manage it. Permanent for the same
// reason as ErrOwnershipMismatch: only an operator can resolve the collision.
var ErrUnmanagedRecord = providerapi.Permanent(errors.New("google cloud dns: unmanaged record collision"))

// Provider implements providerapi.DNSProvider against one Cloud DNS managed zone.
type Provider struct {
	service       *dns.Service
	project, zone string
}

var _ providerapi.DNSProvider = (*Provider)(nil)

// New constructs a Provider for the given Google Cloud project and managed zone
// name, authenticating with Application Default Credentials.
func New(ctx context.Context, project, zone string) (*Provider, error) {
	s, err := dns.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("google cloud dns: creating client: %w", err)
	}
	return &Provider{service: s, project: project, zone: zone}, nil
}
func fqdn(s string) string { return strings.TrimSuffix(s, ".") + "." }

// txt renders value as a Cloud DNS TXT rrdata value: a quoted string with any
// embedded quote escaped.
func txt(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// untxt is the inverse of txt. The surrounding quote pair is stripped before the
// escapes are resolved, never after: unescaping first turns a value that ends in
// an escaped quote into one that ends in a bare quote, which the trim then eats.
func untxt(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, `\"`, `"`)
}
func (p *Provider) records(ctx context.Context, name, typ string) ([]string, *dns.ResourceRecordSet, error) {
	call := p.service.ResourceRecordSets.List(p.project, p.zone).Name(fqdn(name)).Type(typ).Context(ctx)
	page, err := call.Do()
	if err != nil {
		return nil, nil, err
	}
	for _, r := range page.Rrsets {
		if r.Name == fqdn(name) && r.Type == typ {
			return r.Rrdatas, r, nil
		}
	}
	return nil, nil, nil
}
func (p *Provider) apply(ctx context.Context, old, next *dns.ResourceRecordSet) error {
	ch := &dns.Change{}
	if old != nil {
		ch.Deletions = []*dns.ResourceRecordSet{old}
	}
	if next != nil {
		ch.Additions = []*dns.ResourceRecordSet{next}
	}
	_, err := p.service.Changes.Create(p.project, p.zone, ch).Context(ctx).Do()
	return err
}
func vals(in []string, typ string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		if typ == "TXT" {
			out[i] = txt(v)
		} else {
			out[i] = v
		}
	}
	return out
}
func plain(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = untxt(v)
	}
	return out
}
func contains(in []string, v string) bool {
	for _, x := range plain(in) {
		if x == v {
			return true
		}
	}
	return false
}

// remove returns in without the value v. It allocates rather than filtering in
// place: in is the live Rrdatas slice of the record set that callers also pass as
// the Change deletion, and rewriting that array under it makes Cloud DNS reject
// the deletion for not matching the record it is deleting.
func remove(in []string, v string) []string {
	out := make([]string, 0, len(in))
	for _, x := range in {
		if untxt(x) != v {
			out = append(out, x)
		}
	}
	return out
}

// sameValues reports whether set already holds exactly the single value.
func sameValues(existing []string, value string) bool {
	return len(existing) == 1 && untxt(existing[0]) == value
}

// EnsureRecord implements providerapi.DNSProvider. For an A/CNAME record it
// creates or replaces the record and a paired TXT ownership record at the same
// name, refusing to touch a name that already holds a record Ramify does not own.
// For a TXT record — used for the ACME DNS-01 challenge — the value is added
// alongside any existing values, since one name legitimately carries several.
func (p *Provider) EnsureRecord(ctx context.Context, rec providerapi.DNSRecord) error {
	if rec.Type != "A" && rec.Type != "CNAME" && rec.Type != "TXT" {
		return fmt.Errorf("google cloud dns: unsupported record type %s", rec.Type)
	}
	existing, set, err := p.records(ctx, rec.Name, rec.Type)
	if err != nil {
		return err
	}
	if rec.Type == "TXT" {
		if contains(existing, rec.Value) {
			return nil
		}
		existing = append(existing, rec.Value)
		return p.apply(ctx, set, &dns.ResourceRecordSet{Name: fqdn(rec.Name), Type: "TXT", Ttl: 60, Rrdatas: vals(existing, "TXT")})
	}
	owners, ownerSet, err := p.records(ctx, rec.Name, "TXT")
	if err != nil {
		return err
	}
	owned := contains(owners, rec.OwnershipTag)
	if len(existing) > 0 && !owned {
		return ErrUnmanagedRecord
	}
	// Skip the change when the record already holds exactly this value: Cloud DNS
	// accepts a no-op delete-and-re-add, but it costs a change transaction and an
	// entry in the zone's change history on every reconcile.
	if !sameValues(existing, rec.Value) {
		if err := p.apply(ctx, set, &dns.ResourceRecordSet{Name: fqdn(rec.Name), Type: rec.Type, Ttl: 60, Rrdatas: []string{rec.Value}}); err != nil {
			return err
		}
	}
	if !owned {
		owners = append(owners, rec.OwnershipTag)
		return p.apply(ctx, ownerSet, &dns.ResourceRecordSet{Name: fqdn(rec.Name), Type: "TXT", Ttl: 60, Rrdatas: vals(owners, "TXT")})
	}
	return nil
}

// DeleteRecord implements providerapi.DNSProvider, refusing to delete anything not
// vouched for by a matching TXT ownership record at the same name.
func (p *Provider) DeleteRecord(ctx context.Context, rec providerapi.DNSRecord) error {
	owners, ownerSet, err := p.records(ctx, rec.Name, "TXT")
	if err != nil {
		return err
	}
	owner := rec.OwnershipTag
	if rec.Type == "TXT" {
		owner = rec.Value
	}
	if !contains(owners, owner) {
		return ErrOwnershipMismatch
	}
	if rec.Type != "TXT" {
		values, set, err := p.records(ctx, rec.Name, rec.Type)
		if err != nil {
			return err
		}
		values = remove(values, rec.Value)
		if len(values) == 0 {
			if err := p.apply(ctx, set, nil); err != nil {
				return err
			}
		} else if err := p.apply(ctx, set, &dns.ResourceRecordSet{Name: fqdn(rec.Name), Type: rec.Type, Ttl: 60, Rrdatas: values}); err != nil {
			return err
		}
	}
	owners = remove(owners, owner)
	if len(owners) == 0 {
		return p.apply(ctx, ownerSet, nil)
	}
	return p.apply(ctx, ownerSet, &dns.ResourceRecordSet{Name: fqdn(rec.Name), Type: "TXT", Ttl: 60, Rrdatas: vals(owners, "TXT")})
}

// ListManagedRecords implements providerapi.DNSProvider, returning every A/CNAME
// record in zone that has a paired TXT ownership record at the same name.
func (p *Provider) ListManagedRecords(ctx context.Context, zone string) ([]providerapi.DNSRecord, error) {
	var out []providerapi.DNSRecord
	pageToken := ""
	owners := map[string]string{}
	var all []*dns.ResourceRecordSet
	for {
		call := p.service.ResourceRecordSets.List(p.project, p.zone)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		page, err := call.Do()
		if err != nil {
			return nil, err
		}
		all = append(all, page.Rrsets...)
		pageToken = page.NextPageToken
		if pageToken == "" {
			break
		}
	}
	for _, r := range all {
		if r.Type == "TXT" && len(r.Rrdatas) > 0 {
			owners[r.Name] = untxt(r.Rrdatas[0])
		}
	}
	for _, r := range all {
		if r.Type == "TXT" || len(r.Rrdatas) == 0 {
			continue
		}
		if tag, ok := owners[r.Name]; ok {
			out = append(out, providerapi.DNSRecord{Zone: zone, Name: strings.TrimSuffix(r.Name, "."), Type: r.Type, Value: untxt(r.Rrdatas[0]), OwnershipTag: tag})
		}
	}
	return out, nil
}
