// SPDX-License-Identifier: Apache-2.0

// Package route53 implements providerapi.DNSProvider against Amazon Route 53,
// using a TXT ownership registry in the style of the external-dns project: for
// every A/CNAME record Ramify manages at a name, a TXT record at that same name
// carries the OwnershipTag, and DeleteRecord refuses to act unless that TXT
// content matches.
//
// Credentials come from the standard AWS SDK credential chain, so an operator
// configures this the same way they configure the AWS CLI.
package route53

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// ErrOwnershipMismatch is returned by DeleteRecord when no TXT record at the given
// name carries a matching OwnershipTag. It is permanent: the record belongs to
// someone else, and retrying will never make deleting it safe.
var ErrOwnershipMismatch = providerapi.Permanent(errors.New("route53: ownership tag mismatch"))

// ErrUnmanagedRecord indicates that a record already exists at a name but is not
// owned by the Ramify instance attempting to manage it. Permanent for the same
// reason as ErrOwnershipMismatch: only an operator can resolve the collision.
var ErrUnmanagedRecord = providerapi.Permanent(errors.New("route53: unmanaged record collision"))

// Provider implements providerapi.DNSProvider against Route 53. A non-empty
// hostedZoneID pins every operation to that zone; left empty, the zone is resolved
// by name on each call.
type Provider struct {
	client       route53API
	hostedZoneID string
}

// route53API is the subset of *route53.Client this package uses, narrowed to an
// interface so tests can substitute an in-memory fake instead of calling AWS.
type route53API interface {
	ListHostedZonesByName(ctx context.Context, in *route53.ListHostedZonesByNameInput, opts ...func(*route53.Options)) (*route53.ListHostedZonesByNameOutput, error)
	ListResourceRecordSets(ctx context.Context, in *route53.ListResourceRecordSetsInput, opts ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error)
	ChangeResourceRecordSets(ctx context.Context, in *route53.ChangeResourceRecordSetsInput, opts ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error)
}

var _ providerapi.DNSProvider = (*Provider)(nil)

// New constructs a Provider using the standard AWS SDK credential chain.
// hostedZoneID is optional and may be given with or without the "/hostedzone/"
// prefix; when empty, zones are looked up by name.
func New(ctx context.Context, hostedZoneID string) (*Provider, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("route53: loading AWS config: %w", err)
	}
	return &Provider{client: route53.NewFromConfig(cfg), hostedZoneID: strings.TrimPrefix(hostedZoneID, "/hostedzone/")}, nil
}

// newWithClient constructs a Provider over an arbitrary Route 53 API, for tests.
func newWithClient(client route53API, hostedZoneID string) *Provider {
	return &Provider{client: client, hostedZoneID: strings.TrimPrefix(hostedZoneID, "/hostedzone/")}
}

func (p *Provider) zoneID(ctx context.Context, zone string) (string, error) {
	if p.hostedZoneID != "" {
		return p.hostedZoneID, nil
	}
	out, err := p.client.ListHostedZonesByName(ctx, &route53.ListHostedZonesByNameInput{DNSName: aws.String(fqdn(zone))})
	if err != nil {
		return "", err
	}
	for _, z := range out.HostedZones {
		if strings.TrimSuffix(aws.ToString(z.Name), ".") == strings.TrimSuffix(zone, ".") {
			return strings.TrimPrefix(aws.ToString(z.Id), "/hostedzone/"), nil
		}
	}
	return "", fmt.Errorf("hosted zone %q not found", zone)
}

func fqdn(s string) string { return strings.TrimSuffix(s, ".") + "." }

// txt renders value as a Route 53 TXT record value: a quoted string with any
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

func (p *Provider) records(ctx context.Context, zone, name, typ string) ([]types.ResourceRecord, error) {
	id, err := p.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}
	out, err := p.client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{HostedZoneId: aws.String(id), StartRecordName: aws.String(fqdn(name)), StartRecordType: types.RRType(typ)})
	if err != nil {
		return nil, err
	}
	for _, set := range out.ResourceRecordSets {
		if strings.TrimSuffix(aws.ToString(set.Name), ".") == strings.TrimSuffix(name, ".") && string(set.Type) == typ {
			return set.ResourceRecords, nil
		}
	}
	return nil, nil
}

func (p *Provider) change(ctx context.Context, zone, action, name, typ string, values []types.ResourceRecord) error {
	id, err := p.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	_, err = p.client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{HostedZoneId: aws.String(id), ChangeBatch: &types.ChangeBatch{Changes: []types.Change{{Action: types.ChangeAction(action), ResourceRecordSet: &types.ResourceRecordSet{Name: aws.String(fqdn(name)), Type: types.RRType(typ), TTL: aws.Int64(60), ResourceRecords: values}}}}})
	return err
}

func hasValue(rs []types.ResourceRecord, value string) bool {
	for _, r := range rs {
		if untxt(aws.ToString(r.Value)) == value {
			return true
		}
	}
	return false
}
func one(value string, typ string) types.ResourceRecord {
	if typ == "TXT" {
		value = txt(value)
	}
	return types.ResourceRecord{Value: aws.String(value)}
}

// EnsureRecord implements providerapi.DNSProvider. For an A/CNAME record it
// upserts the record and a paired TXT ownership record at the same name, refusing
// to touch a name that already holds a record Ramify does not own. For a TXT
// record — used for the ACME DNS-01 challenge — the value is added alongside any
// existing values, since one name legitimately carries several.
func (p *Provider) EnsureRecord(ctx context.Context, rec providerapi.DNSRecord) error {
	if rec.Type != "A" && rec.Type != "CNAME" && rec.Type != "TXT" {
		return fmt.Errorf("route53: unsupported record type %s", rec.Type)
	}
	values, err := p.records(ctx, rec.Zone, rec.Name, rec.Type)
	if err != nil {
		return fmt.Errorf("route53: listing %s: %w", rec.Name, err)
	}
	if rec.Type == "TXT" {
		if !hasValue(values, rec.Value) {
			values = append(values, one(rec.Value, "TXT"))
			if err := p.change(ctx, rec.Zone, "UPSERT", rec.Name, "TXT", values); err != nil {
				return err
			}
		}
		return nil
	}
	owners, err := p.records(ctx, rec.Zone, rec.Name, "TXT")
	if err != nil {
		return err
	}
	owned := hasValue(owners, rec.OwnershipTag)
	if len(values) > 0 && !owned {
		return fmt.Errorf("%w: %s", ErrUnmanagedRecord, rec.Name)
	}
	if err := p.change(ctx, rec.Zone, "UPSERT", rec.Name, rec.Type, []types.ResourceRecord{one(rec.Value, rec.Type)}); err != nil {
		return fmt.Errorf("route53: upserting %s: %w", rec.Name, err)
	}
	if !owned {
		return p.change(ctx, rec.Zone, "UPSERT", rec.Name, "TXT", append(owners, one(rec.OwnershipTag, "TXT")))
	}
	return nil
}

// DeleteRecord implements providerapi.DNSProvider, refusing to delete anything
// not vouched for by a matching TXT ownership record at the same name.
func (p *Provider) DeleteRecord(ctx context.Context, rec providerapi.DNSRecord) error {
	owners, err := p.records(ctx, rec.Zone, rec.Name, "TXT")
	if err != nil {
		return err
	}
	if !hasValue(owners, rec.OwnershipTag) && rec.Type != "TXT" {
		return ErrOwnershipMismatch
	}
	if rec.Type == "TXT" {
		if !hasValue(owners, rec.Value) {
			return ErrOwnershipMismatch
		}
		previous := append([]types.ResourceRecord(nil), owners...)
		owners = removeValue(owners, rec.Value)
		if len(owners) == 0 {
			return p.change(ctx, rec.Zone, "DELETE", rec.Name, "TXT", previous)
		}
		return p.change(ctx, rec.Zone, "UPSERT", rec.Name, "TXT", owners)
	}
	values, err := p.records(ctx, rec.Zone, rec.Name, rec.Type)
	if err != nil {
		return err
	}
	values = removeValue(values, rec.Value)
	if len(values) == 0 {
		// A record that is already gone is successful idempotent cleanup, but any
		// other failure must surface: swallowing it leaves the record live while
		// Ramify records the environment as torn down.
		if err := p.change(ctx, rec.Zone, "DELETE", rec.Name, rec.Type, []types.ResourceRecord{one(rec.Value, rec.Type)}); err != nil && !isNotFound(err) {
			return fmt.Errorf("route53: deleting %s %s: %w", rec.Type, rec.Name, err)
		}
	} else {
		if err := p.change(ctx, rec.Zone, "UPSERT", rec.Name, rec.Type, values); err != nil {
			return err
		}
	}
	owners = removeValue(owners, rec.OwnershipTag)
	if len(owners) == 0 {
		return p.change(ctx, rec.Zone, "DELETE", rec.Name, "TXT", []types.ResourceRecord{one(rec.OwnershipTag, "TXT")})
	}
	return p.change(ctx, rec.Zone, "UPSERT", rec.Name, "TXT", owners)
}

// removeValue returns rs without the record whose value is value. It allocates
// rather than filtering in place: several callers still read the original slice
// afterwards, and reusing its backing array would silently corrupt them.
func removeValue(rs []types.ResourceRecord, value string) []types.ResourceRecord {
	out := make([]types.ResourceRecord, 0, len(rs))
	for _, r := range rs {
		if untxt(aws.ToString(r.Value)) != value {
			out = append(out, r)
		}
	}
	return out
}

// isNotFound reports whether err is Route 53 refusing a DELETE for a record set
// that does not exist, which destructive reconciliation treats as already done.
func isNotFound(err error) bool {
	var notFound *types.InvalidChangeBatch
	if errors.As(err, &notFound) {
		return true
	}
	var noSuchSet *types.NoSuchHostedZone
	return errors.As(err, &noSuchSet)
}

// ListManagedRecords implements providerapi.DNSProvider, returning every A/CNAME
// record in zone that has a paired TXT ownership record at the same name.
func (p *Provider) ListManagedRecords(ctx context.Context, zone string) ([]providerapi.DNSRecord, error) {
	id, err := p.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}
	sets, err := p.allRecordSets(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("route53: list managed records in %s: %w", zone, err)
	}
	owners := map[string]string{}
	for _, s := range sets {
		if s.Type == types.RRTypeTxt && len(s.ResourceRecords) > 0 {
			owners[aws.ToString(s.Name)] = untxt(aws.ToString(s.ResourceRecords[0].Value))
		}
	}
	var result []providerapi.DNSRecord
	for _, s := range sets {
		if s.Type == types.RRTypeTxt || len(s.ResourceRecords) == 0 {
			continue
		}
		if tag, ok := owners[aws.ToString(s.Name)]; ok {
			result = append(result, providerapi.DNSRecord{Zone: zone, Name: strings.TrimSuffix(aws.ToString(s.Name), "."), Type: string(s.Type), Value: untxt(aws.ToString(s.ResourceRecords[0].Value)), OwnershipTag: tag})
		}
	}
	return result, nil
}

// allRecordSets walks every page of a zone's record sets. Route 53 caps a single
// response at 300 record sets, so a one-shot call silently under-reports a zone
// larger than that — and an orphan sweep that cannot see a record will never
// reclaim it.
func (p *Provider) allRecordSets(ctx context.Context, zoneID string) ([]types.ResourceRecordSet, error) {
	var out []types.ResourceRecordSet
	in := &route53.ListResourceRecordSetsInput{HostedZoneId: aws.String(zoneID)}
	for {
		page, err := p.client.ListResourceRecordSets(ctx, in)
		if err != nil {
			return nil, err
		}
		out = append(out, page.ResourceRecordSets...)
		if !page.IsTruncated {
			return out, nil
		}
		in.StartRecordName = page.NextRecordName
		in.StartRecordType = page.NextRecordType
		in.StartRecordIdentifier = page.NextRecordIdentifier
	}
}
