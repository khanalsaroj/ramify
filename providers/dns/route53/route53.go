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

var ErrOwnershipMismatch = providerapi.Permanent(errors.New("route53: ownership tag mismatch"))
var ErrUnmanagedRecord = providerapi.Permanent(errors.New("route53: unmanaged record collision"))

type Provider struct {
	client       *route53.Client
	hostedZoneID string
}

var _ providerapi.DNSProvider = (*Provider)(nil)

func New(ctx context.Context, hostedZoneID string) (*Provider, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("route53: loading AWS config: %w", err)
	}
	return &Provider{client: route53.NewFromConfig(cfg), hostedZoneID: strings.TrimPrefix(hostedZoneID, "/hostedzone/")}, nil
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

func fqdn(s string) string  { return strings.TrimSuffix(s, ".") + "." }
func txt(s string) string   { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }
func untxt(s string) string { return strings.Trim(strings.ReplaceAll(s, `\"`, `"`), `"`) }

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
		_ = p.change(ctx, rec.Zone, "DELETE", rec.Name, rec.Type, []types.ResourceRecord{one(rec.Value, rec.Type)})
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

func removeValue(rs []types.ResourceRecord, value string) []types.ResourceRecord {
	out := rs[:0]
	for _, r := range rs {
		if untxt(aws.ToString(r.Value)) != value {
			out = append(out, r)
		}
	}
	return out
}

func (p *Provider) ListManagedRecords(ctx context.Context, zone string) ([]providerapi.DNSRecord, error) {
	id, err := p.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}
	out, err := p.client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{HostedZoneId: aws.String(id)})
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	for _, s := range out.ResourceRecordSets {
		if s.Type == types.RRTypeTxt && len(s.ResourceRecords) > 0 {
			owners[aws.ToString(s.Name)] = untxt(aws.ToString(s.ResourceRecords[0].Value))
		}
	}
	var result []providerapi.DNSRecord
	for _, s := range out.ResourceRecordSets {
		if s.Type == types.RRTypeTxt || len(s.ResourceRecords) == 0 {
			continue
		}
		if tag, ok := owners[aws.ToString(s.Name)]; ok {
			result = append(result, providerapi.DNSRecord{Zone: zone, Name: strings.TrimSuffix(aws.ToString(s.Name), "."), Type: string(s.Type), Value: untxt(aws.ToString(s.ResourceRecords[0].Value)), OwnershipTag: tag})
		}
	}
	return result, nil
}
