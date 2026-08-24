// SPDX-License-Identifier: Apache-2.0

// Package acme implements providerapi.CertificateProvider using go-acme/lego,
// solving the ACME DNS-01 challenge through a Ramify DNSProvider — normally the
// same Cloudflare provider used for the environment's own A/CNAME record.
package acme

import (
	"context"
	"crypto"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// Config configures a Provider.
type Config struct {
	// CADirURL is the ACME directory URL, e.g. lego.LEDirectoryProduction, or a
	// local Pebble instance's URL for testing.
	CADirURL string
	// Email is the contact address registered with the ACME account.
	Email string
	// Zone is the DNS zone the DNS01 challenge's TXT record is created in.
	Zone string
	// DNSProvider solves the DNS-01 challenge by writing (and cleaning up) the
	// "_acme-challenge" TXT record.
	DNSProvider providerapi.DNSProvider
	// SkipPropagationCheck disables lego's active DNS propagation polling before
	// asking the CA to validate, waiting a short fixed delay instead. Intended for
	// test harnesses (Pebble + CoreDNS) where propagation is effectively instant;
	// production use against a real DNS provider should leave this false.
	SkipPropagationCheck bool
	// HTTPClient, if set, is used for all ACME directory/order/challenge HTTP
	// calls — needed in test harnesses to trust a self-signed CA (Pebble).
	HTTPClient *http.Client
}

// acmeUser implements registration.User.
type acmeUser struct {
	email        string
	key          crypto.PrivateKey
	registration *registration.Resource
}

// GetEmail implements registration.User.
func (u *acmeUser) GetEmail() string { return u.email }

// GetRegistration implements registration.User.
func (u *acmeUser) GetRegistration() *registration.Resource { return u.registration }

// GetPrivateKey implements registration.User.
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey { return u.key }

// issuedCert is what Provider caches per domain between EnsureCertificate and a
// later RevokeCertificate call.
type issuedCert struct {
	ref  providerapi.CertRef
	pem  []byte
	leaf *time.Time
}

// Provider implements providerapi.CertificateProvider.
type Provider struct {
	client *lego.Client
	zone   string

	mu    sync.Mutex
	cache map[string]issuedCert // domain -> issued certificate
}

var _ providerapi.CertificateProvider = (*Provider)(nil)

// New constructs a Provider, generating a fresh ACME account key and registering it
// with the CA at cfg.CADirURL.
func New(cfg Config) (*Provider, error) {
	key, err := certcrypto.GeneratePrivateKey(certcrypto.EC256)
	if err != nil {
		return nil, fmt.Errorf("acme: generating account key: %w", err)
	}

	user := &acmeUser{email: cfg.Email, key: key}
	legoConfig := lego.NewConfig(user)
	legoConfig.CADirURL = cfg.CADirURL
	if cfg.HTTPClient != nil {
		legoConfig.HTTPClient = cfg.HTTPClient
	}

	client, err := lego.NewClient(legoConfig)
	if err != nil {
		return nil, fmt.Errorf("acme: constructing client: %w", err)
	}

	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, fmt.Errorf("acme: registering account: %w", err)
	}
	user.registration = reg

	adapter := &dnsChallengeAdapter{dns: cfg.DNSProvider, zone: cfg.Zone}
	var opts []dns01.ChallengeOption
	if cfg.SkipPropagationCheck {
		opts = append(opts, dns01.PropagationWait(2*time.Second, true))
	}
	if err := client.Challenge.SetDNS01Provider(adapter, opts...); err != nil {
		return nil, fmt.Errorf("acme: configuring dns-01 challenge: %w", err)
	}

	return &Provider{client: client, zone: cfg.Zone, cache: make(map[string]issuedCert)}, nil
}

// EnsureCertificate implements providerapi.CertificateProvider. It returns a cached
// certificate for domain if one is already issued and not within 30 days of expiry,
// or obtains a new one via the ACME DNS-01 flow.
func (p *Provider) EnsureCertificate(_ context.Context, domain string) (providerapi.CertRef, error) {
	p.mu.Lock()
	if cached, ok := p.cache[domain]; ok && cached.leaf != nil && time.Until(*cached.leaf) > 30*24*time.Hour {
		p.mu.Unlock()
		return cached.ref, nil
	}
	p.mu.Unlock()

	res, err := p.client.Certificate.Obtain(certificate.ObtainRequest{
		Domains:        []string{domain},
		Bundle:         true,
		EmailAddresses: nil,
	})
	if err != nil {
		return providerapi.CertRef{}, fmt.Errorf("acme: obtaining certificate for %s: %w", domain, err)
	}

	leafExpiry, err := leafNotAfter(res.Certificate)
	if err != nil {
		return providerapi.CertRef{}, fmt.Errorf("acme: parsing issued certificate for %s: %w", domain, err)
	}

	ref := providerapi.CertRef{Domain: domain, ExpiresAt: leafExpiry.Format(time.RFC3339)}

	p.mu.Lock()
	p.cache[domain] = issuedCert{ref: ref, pem: res.Certificate, leaf: &leafExpiry}
	p.mu.Unlock()

	return ref, nil
}

// RevokeCertificate implements providerapi.CertificateProvider. Revoking a domain
// with no cached certificate (already revoked, or never issued in this process) is
// a no-op, matching the idempotent-teardown requirement.
func (p *Provider) RevokeCertificate(_ context.Context, domain string) error {
	p.mu.Lock()
	cached, ok := p.cache[domain]
	p.mu.Unlock()
	if !ok {
		return nil
	}

	if err := p.client.Certificate.Revoke(cached.pem); err != nil {
		return fmt.Errorf("acme: revoking certificate for %s: %w", domain, err)
	}

	p.mu.Lock()
	delete(p.cache, domain)
	p.mu.Unlock()
	return nil
}

func leafNotAfter(pemBundle []byte) (time.Time, error) {
	certs, err := certcrypto.ParsePEMBundle(pemBundle)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing pem bundle: %w", err)
	}
	if len(certs) == 0 {
		return time.Time{}, fmt.Errorf("pem bundle contained no certificates")
	}
	return certs[0].NotAfter, nil
}

// dnsChallengeAdapter implements challenge.Provider (Present/CleanUp), solving the
// ACME DNS-01 challenge by writing a self-owned TXT record via a Ramify
// DNSProvider. lego's challenge.Provider interface has no context.Context
// parameter, so context.Background() is used for the underlying DNSProvider calls;
// this is an external constraint of the lego API, not a deviation from Ramify's own
// I/O-context convention.
type dnsChallengeAdapter struct {
	dns  providerapi.DNSProvider
	zone string
}

// Present implements challenge.Provider.
func (a *dnsChallengeAdapter) Present(domain, _, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	err := a.dns.EnsureRecord(context.Background(), providerapi.DNSRecord{
		Zone: a.zone, Name: info.FQDN, Type: "TXT", Value: info.Value, OwnershipTag: "acme-dns01-" + domain,
	})
	if err != nil {
		return fmt.Errorf("acme: presenting dns-01 challenge for %s: %w", domain, err)
	}
	return nil
}

// CleanUp implements challenge.Provider.
func (a *dnsChallengeAdapter) CleanUp(domain, _, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	err := a.dns.DeleteRecord(context.Background(), providerapi.DNSRecord{
		Zone: a.zone, Name: info.FQDN, Type: "TXT", Value: info.Value, OwnershipTag: "acme-dns01-" + domain,
	})
	if err != nil {
		return fmt.Errorf("acme: cleaning up dns-01 challenge for %s: %w", domain, err)
	}
	return nil
}
