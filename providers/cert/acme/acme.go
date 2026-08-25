// SPDX-License-Identifier: Apache-2.0

// Package acme implements providerapi.CertificateProvider using go-acme/lego,
// solving the ACME DNS-01 challenge through a Ramify DNSProvider — normally the
// same Cloudflare provider used for the environment's own A/CNAME record.
package acme

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	// StorageDir persists issued certificate material so restart does not lose
	// the ability to reuse or revoke certificates.
	StorageDir string
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
	key  []byte
	leaf *time.Time
}

// Provider implements providerapi.CertificateProvider.
type Provider struct {
	client     *lego.Client
	zone       string
	storageDir string

	mu      sync.Mutex
	issueMu sync.Mutex
	cache   map[string]issuedCert // domain -> issued certificate
}

var _ providerapi.CertificateProvider = (*Provider)(nil)

// New constructs a Provider, generating a fresh ACME account key and registering it
// with the CA at cfg.CADirURL.
func New(cfg Config) (*Provider, error) {
	if cfg.StorageDir == "" {
		return nil, fmt.Errorf("acme: storage directory is required")
	}
	if err := os.MkdirAll(cfg.StorageDir, 0o700); err != nil {
		return nil, fmt.Errorf("acme: creating storage directory: %w", err)
	}
	key, generated, err := loadOrGenerateAccountKey(cfg.StorageDir)
	if err != nil {
		return nil, fmt.Errorf("acme: generating account key: %w", err)
	}
	if generated {
		if err := persistAccountKey(cfg.StorageDir, key); err != nil {
			return nil, err
		}
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

	reg, err := client.Registration.ResolveAccountByKey()
	if err != nil {
		reg, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, fmt.Errorf("acme: registering account: %w", err)
		}
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

	p := &Provider{client: client, zone: cfg.Zone, storageDir: cfg.StorageDir, cache: make(map[string]issuedCert)}
	if err := p.loadCache(); err != nil {
		return nil, err
	}
	return p, nil
}

func accountKeyPath(storageDir string) string {
	return filepath.Join(storageDir, "account-key.pem")
}

func loadOrGenerateAccountKey(storageDir string) (crypto.PrivateKey, bool, error) {
	data, err := os.ReadFile(accountKeyPath(storageDir))
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, false, fmt.Errorf("acme: decoding persisted account key: no PEM block")
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, false, fmt.Errorf("acme: parsing persisted account key: %w", err)
		}
		return key, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("acme: reading persisted account key: %w", err)
	}
	key, err := certcrypto.GeneratePrivateKey(certcrypto.EC256)
	if err != nil {
		return nil, false, err
	}
	return key, true, nil
}

func persistAccountKey(storageDir string, key crypto.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("acme: encoding account key: %w", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(accountKeyPath(storageDir), data, 0o600); err != nil {
		return fmt.Errorf("acme: persisting account key: %w", err)
	}
	return nil
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

	p.issueMu.Lock()
	defer p.issueMu.Unlock()
	// Another concurrent caller may have filled the cache while this caller
	// waited for the issuance lock.
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

	ref := providerapi.CertRef{Domain: domain, ExpiresAt: leafExpiry.Format(time.RFC3339), CertificatePEM: res.Certificate, PrivateKeyPEM: res.PrivateKey}

	issued := issuedCert{ref: ref, pem: res.Certificate, key: res.PrivateKey, leaf: &leafExpiry}
	if err := p.persist(domain, issued); err != nil {
		return providerapi.CertRef{}, err
	}
	p.mu.Lock()
	p.cache[domain] = issued
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
	if err := os.Remove(p.cachePath(domain)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("acme: removing persisted certificate for %s: %w", domain, err)
	}
	return nil
}

type persistedCertificate struct {
	Domain     string              `json:"domain"`
	Ref        providerapi.CertRef `json:"ref"`
	PEM        []byte              `json:"certificate_pem"`
	PrivateKey []byte              `json:"private_key_pem"`
	ExpiresAt  time.Time           `json:"expires_at"`
}

func (p *Provider) cachePath(domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return filepath.Join(p.storageDir, hex.EncodeToString(sum[:])+".json")
}

func (p *Provider) persist(domain string, cert issuedCert) error {
	data, err := json.Marshal(persistedCertificate{Domain: domain, Ref: cert.ref, PEM: cert.pem, PrivateKey: cert.key, ExpiresAt: *cert.leaf})
	if err != nil {
		return fmt.Errorf("acme: encoding persisted certificate %s: %w", domain, err)
	}
	tmp, err := os.CreateTemp(p.storageDir, ".certificate-*.tmp")
	if err != nil {
		return fmt.Errorf("acme: creating certificate temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("acme: securing certificate temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("acme: writing persisted certificate: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("acme: closing persisted certificate: %w", err)
	}
	if err := os.Rename(tmpName, p.cachePath(domain)); err != nil {
		return fmt.Errorf("acme: replacing persisted certificate: %w", err)
	}
	return nil
}

func (p *Provider) loadCache() error {
	entries, err := os.ReadDir(p.storageDir)
	if err != nil {
		return fmt.Errorf("acme: reading certificate storage: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.storageDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("acme: reading persisted certificate %s: %w", entry.Name(), err)
		}
		var saved persistedCertificate
		if err := json.Unmarshal(data, &saved); err != nil {
			return fmt.Errorf("acme: decoding persisted certificate %s: %w", entry.Name(), err)
		}
		if saved.Domain == "" || len(saved.PEM) == 0 || saved.ExpiresAt.IsZero() {
			continue
		}
		leaf := saved.ExpiresAt
		saved.Ref.CertificatePEM = saved.PEM
		saved.Ref.PrivateKeyPEM = saved.PrivateKey
		p.cache[saved.Domain] = issuedCert{ref: saved.Ref, pem: saved.PEM, key: saved.PrivateKey, leaf: &leaf}
	}
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
