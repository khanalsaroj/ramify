// SPDX-License-Identifier: Apache-2.0

package route53

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
)

// fakeRoute53 is an in-memory stand-in for the Route 53 API, keyed the way Route 53
// itself is: one record set per (name, type), each holding one or more values.
type fakeRoute53 struct {
	sets map[string]*types.ResourceRecordSet

	// pageSize, when non-zero, truncates ListResourceRecordSets responses so
	// pagination handling can be exercised.
	pageSize int
	listCall int
}

func newFakeRoute53() *fakeRoute53 {
	return &fakeRoute53{sets: map[string]*types.ResourceRecordSet{}}
}

func setKey(name string, typ types.RRType) string { return fqdn(name) + "|" + string(typ) }

func (f *fakeRoute53) ListHostedZonesByName(_ context.Context, in *route53.ListHostedZonesByNameInput, _ ...func(*route53.Options)) (*route53.ListHostedZonesByNameOutput, error) {
	return &route53.ListHostedZonesByNameOutput{HostedZones: []types.HostedZone{{
		Id: aws.String("/hostedzone/Z-FAKE"), Name: in.DNSName,
	}}}, nil
}

func (f *fakeRoute53) sortedSets() []types.ResourceRecordSet {
	keys := make([]string, 0, len(f.sets))
	for k := range f.sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]types.ResourceRecordSet, 0, len(keys))
	for _, k := range keys {
		out = append(out, *f.sets[k])
	}
	return out
}

func (f *fakeRoute53) ListResourceRecordSets(_ context.Context, in *route53.ListResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	f.listCall++
	all := f.sortedSets()

	// Honour StartRecordName the way Route 53 does: begin at the first set at or
	// after the requested name.
	if in.StartRecordName != nil {
		start := aws.ToString(in.StartRecordName)
		for len(all) > 0 && all[0].Name != nil && aws.ToString(all[0].Name) < start {
			all = all[1:]
		}
	}
	if f.pageSize > 0 && len(all) > f.pageSize {
		page := all[:f.pageSize]
		next := all[f.pageSize]
		return &route53.ListResourceRecordSetsOutput{
			ResourceRecordSets: page,
			IsTruncated:        true,
			NextRecordName:     next.Name,
			NextRecordType:     next.Type,
		}, nil
	}
	return &route53.ListResourceRecordSetsOutput{ResourceRecordSets: all}, nil
}

func (f *fakeRoute53) ChangeResourceRecordSets(_ context.Context, in *route53.ChangeResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	for _, change := range in.ChangeBatch.Changes {
		set := change.ResourceRecordSet
		key := setKey(aws.ToString(set.Name), set.Type)
		switch change.Action {
		case types.ChangeActionUpsert, types.ChangeActionCreate:
			stored := *set
			f.sets[key] = &stored
		case types.ChangeActionDelete:
			if _, ok := f.sets[key]; !ok {
				return nil, &types.InvalidChangeBatch{Message: aws.String("record set does not exist")}
			}
			delete(f.sets, key)
		}
	}
	return &route53.ChangeResourceRecordSetsOutput{}, nil
}

func newTestProvider(t *testing.T) (*Provider, *fakeRoute53) {
	t.Helper()
	fake := newFakeRoute53()
	return newWithClient(fake, "Z-FAKE"), fake
}

func TestProviderContract(t *testing.T) {
	p, _ := newTestProvider(t)
	contract.RunDNSProviderContract(t, p, "preview.example.com")
}

func TestEnsureRecordRefusesUnownedName(t *testing.T) {
	p, fake := newTestProvider(t)
	ctx := context.Background()

	// A record an operator created by hand: no Ramify TXT ownership marker.
	fake.sets[setKey("app.preview.example.com", types.RRTypeA)] = &types.ResourceRecordSet{
		Name: aws.String(fqdn("app.preview.example.com")),
		Type: types.RRTypeA,
		ResourceRecords: []types.ResourceRecord{
			{Value: aws.String("198.51.100.1")},
		},
	}

	err := p.EnsureRecord(ctx, providerapi.DNSRecord{
		Zone: "preview.example.com", Name: "app.preview.example.com",
		Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-tag",
	})
	require.ErrorIs(t, err, ErrUnmanagedRecord)
	require.ErrorIs(t, err, providerapi.ErrPermanent)

	// The operator's record must be untouched.
	stored := fake.sets[setKey("app.preview.example.com", types.RRTypeA)]
	require.Equal(t, "198.51.100.1", aws.ToString(stored.ResourceRecords[0].Value))
}

// TestListManagedRecordsPaginates guards the orphan sweep: Route 53 caps a
// response at 300 record sets, and a zone larger than one page must still report
// every managed record.
func TestListManagedRecordsPaginates(t *testing.T) {
	p, fake := newTestProvider(t)
	ctx := context.Background()
	fake.pageSize = 2

	const zone = "preview.example.com"
	names := []string{"a." + zone, "b." + zone, "c." + zone, "d." + zone}
	for _, name := range names {
		require.NoError(t, p.EnsureRecord(ctx, providerapi.DNSRecord{
			Zone: zone, Name: name, Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-tag",
		}))
	}

	records, err := p.ListManagedRecords(ctx, zone)
	require.NoError(t, err)
	require.Len(t, records, len(names), "every page of the zone must be walked")

	got := make([]string, 0, len(records))
	for _, r := range records {
		got = append(got, r.Name)
		require.Equal(t, "ramify-tag", r.OwnershipTag)
	}
	sort.Strings(got)
	require.Equal(t, names, got)
}

// TestDeleteRecordSurfacesChangeFailures verifies a failing DELETE is reported
// rather than swallowed: a caller that is told the record is gone while it is
// still live leaks a preview hostname.
func TestDeleteRecordSurfacesChangeFailures(t *testing.T) {
	p, fake := newTestProvider(t)
	ctx := context.Background()

	rec := providerapi.DNSRecord{
		Zone: "preview.example.com", Name: "app.preview.example.com",
		Type: "A", Value: "203.0.113.10", OwnershipTag: "ramify-tag",
	}
	require.NoError(t, p.EnsureRecord(ctx, rec))

	failing := &failingChangeRoute53{fakeRoute53: fake}
	p.client = failing
	require.Error(t, p.DeleteRecord(ctx, rec))
}

type failingChangeRoute53 struct {
	*fakeRoute53
}

func (f *failingChangeRoute53) ChangeResourceRecordSets(_ context.Context, _ *route53.ChangeResourceRecordSetsInput, _ ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	return nil, &types.PriorRequestNotComplete{Message: aws.String("throttled")}
}

func TestTXTRoundTripPreservesQuoting(t *testing.T) {
	value := `heritage=ramify,owner="preview"`
	require.Equal(t, value, untxt(txt(value)))
	require.True(t, strings.HasPrefix(txt(value), `"`))
}
