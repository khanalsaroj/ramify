// SPDX-License-Identifier: Apache-2.0

package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/test/fakes"
)

// Provider.New dials the ACME CA directory as part of client construction and
// account registration, so exercising it end to end requires a live ACME server
// (Pebble, in the e2e harness — see docs/providers.md and test/e2e). Per §7 item 1,
// unit tests here cover everything that doesn't require that live server: the
// DNS-01 challenge adapter and the certificate-parsing helper.

func TestDNSChallengeAdapterPresentAndCleanUp(t *testing.T) {
	dns := fakes.NewDNSProvider()
	adapter := &dnsChallengeAdapter{dns: dns, zone: "preview.example.com"}

	require.NoError(t, adapter.Present("feature-x.preview.example.com", "token123", "key-auth-value"))

	records, err := dns.ListManagedRecords(context.Background(), "preview.example.com")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "_acme-challenge.feature-x.preview.example.com.", records[0].Name)
	require.Equal(t, "TXT", records[0].Type)
	require.NotEmpty(t, records[0].Value)

	require.NoError(t, adapter.CleanUp("feature-x.preview.example.com", "token123", "key-auth-value"))

	records, err = dns.ListManagedRecords(context.Background(), "preview.example.com")
	require.NoError(t, err)
	require.Empty(t, records)
}

func TestAcmeUserGetters(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	u := &acmeUser{email: "ops@example.com", key: key}
	require.Equal(t, "ops@example.com", u.GetEmail())
	require.Equal(t, key, u.GetPrivateKey())
	require.Nil(t, u.GetRegistration())
}

func TestLeafNotAfter(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "feature-x.preview.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	got, err := leafNotAfter(pemBytes)
	require.NoError(t, err)
	require.True(t, got.Equal(notAfter), "expected %v, got %v", notAfter, got)
}

func TestLeafNotAfterEmptyBundle(t *testing.T) {
	_, err := leafNotAfter(nil)
	require.Error(t, err)
}
