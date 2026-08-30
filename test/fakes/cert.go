// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"context"
	"sync"
	"time"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// CertificateProvider is an in-memory fake of providerapi.CertificateProvider.
type CertificateProvider struct {
	mu    sync.Mutex
	certs map[string]providerapi.CertRef // keyed by domain

	// Now, if set, is used instead of time.Now for computing ExpiresAt.
	Now func() time.Time

	// RevokeErr, when set, is returned by every RevokeCertificate call instead
	// of succeeding, so tests can exercise compensating-cleanup ordering when
	// one teardown step fails.
	RevokeErr error
}

var _ providerapi.CertificateProvider = (*CertificateProvider)(nil)

// NewCertificateProvider returns an empty fake CertificateProvider.
func NewCertificateProvider() *CertificateProvider {
	return &CertificateProvider{certs: make(map[string]providerapi.CertRef)}
}

func (f *CertificateProvider) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// EnsureCertificate implements providerapi.CertificateProvider.
func (f *CertificateProvider) EnsureCertificate(_ context.Context, domain string) (providerapi.CertRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.certs[domain]; ok {
		return existing, nil
	}
	ref := providerapi.CertRef{
		Domain:    domain,
		ExpiresAt: f.now().Add(90 * 24 * time.Hour).Format(time.RFC3339),
	}
	f.certs[domain] = ref
	return ref, nil
}

// RevokeCertificate implements providerapi.CertificateProvider.
func (f *CertificateProvider) RevokeCertificate(_ context.Context, domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RevokeErr != nil {
		return f.RevokeErr
	}
	delete(f.certs, domain)
	return nil
}
