// SPDX-License-Identifier: Apache-2.0

// Package dnsfile implements providerapi.DNSProvider by directly managing a
// BIND-style zone file that CoreDNS's `file` plugin serves. It exists only for
// test/e2e: providers/dns/cloudflare is Ramify's only shipped DNS provider (see
// DECISIONS.md), and the e2e harness has no real Cloudflare account to test
// against, so this stands in for it, applying the same TXT-ownership-registry
// rules (§6 of the build spec) against a local file instead of a remote API.
package dnsfile

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

const (
	beginMarker = "; RAMIFY-MANAGED-BEGIN"
	endMarker   = "; RAMIFY-MANAGED-END"
)

// ErrOwnershipMismatch is returned by DeleteRecord when no TXT record at the given
// name carries a matching ownership marker.
var ErrOwnershipMismatch = fmt.Errorf("dnsfile: ownership tag mismatch, refusing delete")

type record struct {
	name  string // fully-qualified, trailing dot
	typ   string
	value string
}

// Provider implements providerapi.DNSProvider against a local zone file.
type Provider struct {
	mu     sync.Mutex
	path   string
	zone   string
	serial int
}

var _ providerapi.DNSProvider = (*Provider)(nil)

// New constructs a Provider managing the zone file at path for zone (e.g.
// "preview.example.com"). The file must already exist (CoreDNS's file plugin
// requires it to exist at startup); Header can be used to seed it.
func New(path, zone string) *Provider {
	return &Provider{path: path, zone: zone, serial: 1}
}

// Header returns the static (non-record) portion of a fresh zone file for zone,
// suitable for seeding the file CoreDNS reads before Provider ever writes to it.
func Header(zone string) string {
	return zoneHeader(zone, 1)
}

func zoneHeader(zone string, serial int) string {
	return fmt.Sprintf(`$ORIGIN %s.
$TTL 60
@   IN  SOA ns.%s. admin.%s. ( %d 60 60 60 60 )
@   IN  NS  ns.%s.
ns  IN  A   127.0.0.1
`, zone, zone, zone, serial, zone)
}

// EnsureRecord implements providerapi.DNSProvider. For an A/CNAME record it also
// writes a paired ownership TXT record at the same name; for a TXT record (used by
// the ACME DNS-01 challenge) it writes only that record, matching
// providers/dns/cloudflare's TXT self-ownership handling.
func (p *Provider) EnsureRecord(_ context.Context, rec providerapi.DNSRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	records, err := p.readManaged()
	if err != nil {
		return fmt.Errorf("dnsfile: ensure record %s: %w", rec.Name, err)
	}

	fqdn := fqdnOf(rec.Name)
	records = upsert(records, record{name: fqdn, typ: rec.Type, value: rec.Value})
	if rec.Type != "TXT" {
		records = upsert(records, record{name: fqdn, typ: "TXT", value: rec.OwnershipTag})
	}

	if err := p.writeManaged(records); err != nil {
		return fmt.Errorf("dnsfile: ensure record %s: %w", rec.Name, err)
	}
	return nil
}

// DeleteRecord implements providerapi.DNSProvider, mirroring
// providers/dns/cloudflare's ownership-verification rules exactly.
func (p *Provider) DeleteRecord(_ context.Context, rec providerapi.DNSRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	records, err := p.readManaged()
	if err != nil {
		return fmt.Errorf("dnsfile: delete record %s: %w", rec.Name, err)
	}

	fqdn := fqdnOf(rec.Name)
	ownerContent := rec.OwnershipTag
	if rec.Type == "TXT" {
		ownerContent = rec.Value
	}

	owned := false
	for _, r := range records {
		if r.name == fqdn && r.typ == "TXT" && r.value == ownerContent {
			owned = true
			break
		}
	}
	if !owned {
		return fmt.Errorf("dnsfile: delete record %s: %w", rec.Name, ErrOwnershipMismatch)
	}

	var kept []record
	for _, r := range records {
		if r.name != fqdn {
			kept = append(kept, r)
			continue
		}
		if rec.Type == "TXT" {
			if r.typ == "TXT" && r.value == ownerContent {
				continue // drop the self-owned TXT record itself
			}
		} else if r.typ == rec.Type || r.typ == "TXT" {
			continue // drop the main A/CNAME record and its paired ownership TXT
		}
		kept = append(kept, r)
	}

	if err := p.writeManaged(kept); err != nil {
		return fmt.Errorf("dnsfile: delete record %s: %w", rec.Name, err)
	}
	return nil
}

// ListManagedRecords implements providerapi.DNSProvider.
func (p *Provider) ListManagedRecords(_ context.Context, zone string) ([]providerapi.DNSRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	records, err := p.readManaged()
	if err != nil {
		return nil, fmt.Errorf("dnsfile: list managed records: %w", err)
	}

	txtByName := make(map[string]string)
	for _, r := range records {
		if r.typ == "TXT" {
			txtByName[r.name] = r.value
		}
	}

	var out []providerapi.DNSRecord
	for _, r := range records {
		if r.typ == "TXT" {
			continue
		}
		tag, ok := txtByName[r.name]
		if !ok {
			continue
		}
		out = append(out, providerapi.DNSRecord{Zone: zone, Name: strings.TrimSuffix(r.name, "."), Type: r.typ, Value: r.value, OwnershipTag: tag})
	}
	return out, nil
}

func upsert(records []record, rec record) []record {
	for i, r := range records {
		if r.name == rec.name && r.typ == rec.typ {
			records[i].value = rec.value
			return records
		}
	}
	return append(records, rec)
}

func fqdnOf(name string) string {
	return strings.TrimSuffix(name, ".") + "."
}

func (p *Provider) writeManaged(records []record) error {
	p.serial++

	var b strings.Builder
	b.WriteString(zoneHeader(p.zone, p.serial))
	b.WriteString(beginMarker + "\n")
	for _, r := range records {
		value := r.value
		if r.typ == "TXT" {
			value = strconv.Quote(value)
		}
		fmt.Fprintf(&b, "%s IN %s %s\n", r.name, r.typ, value)
	}
	b.WriteString(endMarker + "\n")

	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil { //nolint:gosec // shared test zone file, not sensitive
		return fmt.Errorf("writing zone file: %w", err)
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return fmt.Errorf("renaming zone file: %w", err)
	}
	return nil
}

func (p *Provider) readManaged() ([]record, error) {
	data, err := os.ReadFile(p.path) //nolint:gosec // shared test zone file, configured at process startup
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading zone file: %w", err)
	}

	var records []record
	inBlock := false
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == beginMarker:
			inBlock = true
			continue
		case trimmed == endMarker:
			inBlock = false
			continue
		case !inBlock || trimmed == "":
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 4 {
			continue
		}
		name, typ, value := fields[0], fields[2], strings.Join(fields[3:], " ")
		if typ == "TXT" {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
		}
		records = append(records, record{name: name, typ: typ, value: value})
	}
	return records, nil
}
