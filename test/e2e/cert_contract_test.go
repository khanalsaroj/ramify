// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/cert/acme"
	"github.com/khanalsaroj/ramify/test/contract"
	"github.com/khanalsaroj/ramify/test/e2e/dnsfile"
)

// TestCertificateProviderContract runs the shared CertificateProvider contract
// against the real ACME provider talking to Pebble, the only seam it has — see
// docs/providers.md's "Running the contract suite against a real account" section
// for why the acme package itself can't be exercised against a fake at the unit
// level (lego.NewClient dials the ACME directory as part of construction).
func TestCertificateProviderContract(t *testing.T) {
	e := loadEnv(t)

	httpClient := pebbleHTTPClient(t, e.pebbleCACert)
	waitFor(t, 60*time.Second, "pebble directory reachable", func() error {
		resp, err := httpClient.Get(e.pebbleDirURL) //nolint:gosec,noctx // fixed test-harness URL, one-shot readiness probe
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		return nil
	})

	dnsProvider := dnsfile.New(e.zoneFile, e.zone)
	certProvider, err := acme.New(acme.Config{
		CADirURL:             e.pebbleDirURL,
		Email:                "e2e-cert-contract@example.com",
		Zone:                 e.zone,
		DNSProvider:          dnsProvider,
		SkipPropagationCheck: true,
		HTTPClient:           httpClient,
		StorageDir:           t.TempDir(),
	})
	require.NoError(t, err)

	contract.RunCertificateProviderContract(t, certProvider, "cert-contract."+e.zone)
}
