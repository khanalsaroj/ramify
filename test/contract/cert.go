// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// RunCertificateProviderContract verifies the minimum behavior every
// providerapi.CertificateProvider implementation must satisfy: EnsureCertificate
// issues a certificate for domain and is idempotent (a second call reuses the
// existing certificate rather than reissuing), and RevokeCertificate is
// idempotent.
func RunCertificateProviderContract(t *testing.T, p providerapi.CertificateProvider, domain string) {
	t.Helper()
	ctx := context.Background()

	ref, err := p.EnsureCertificate(ctx, domain)
	require.NoError(t, err)
	require.Equal(t, domain, ref.Domain)
	require.NotEmpty(t, ref.ExpiresAt)

	t.Run("ensure certificate is idempotent", func(t *testing.T) {
		ref2, err := p.EnsureCertificate(ctx, domain)
		require.NoError(t, err)
		require.Equal(t, ref, ref2, "a second EnsureCertificate call must reuse the existing certificate, not reissue")
	})

	t.Run("revoke", func(t *testing.T) {
		require.NoError(t, p.RevokeCertificate(ctx, domain))
	})

	t.Run("revoke is idempotent", func(t *testing.T) {
		require.NoError(t, p.RevokeCertificate(ctx, domain))
	})
}
