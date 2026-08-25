// SPDX-License-Identifier: Apache-2.0

package providerapi

import "context"

// CertRef identifies an issued TLS certificate.
type CertRef struct {
	Domain         string
	ExpiresAt      string // RFC3339
	CertificatePEM []byte `json:"-"`
	PrivateKeyPEM  []byte `json:"-"`
}

// CertificateProvider issues and revokes TLS certificates for preview environment
// domains.
type CertificateProvider interface {
	// EnsureCertificate issues a certificate for domain if one doesn't already
	// exist and isn't near expiry, or returns the existing valid CertRef.
	EnsureCertificate(ctx context.Context, domain string) (CertRef, error)
	// RevokeCertificate revokes the certificate previously issued for domain.
	RevokeCertificate(ctx context.Context, domain string) error
}
