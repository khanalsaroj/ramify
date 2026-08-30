// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// generateSelfSignedCert writes a throwaway self-signed certificate and key for
// "127.0.0.1" to dir, returning their paths, so tests can exercise Serve's TLS
// path without depending on any real CA.
func generateSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPath = filepath.Join(dir, "cert.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))

	keyBytes, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPath = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600))
	return certPath, keyPath
}

// shortSocketPath returns a unix socket path short enough to fit in sockaddr_un's
// sun_path, which is 104 bytes on Darwin and 108 on Linux. t.TempDir() is unusable
// here: on macOS it roots temp dirs under a ~49-character /var/folders/... prefix
// and includes the (long) test name, which overruns the limit and makes Listen fail
// with "invalid argument" for no reason related to what the test is checking.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rmf")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestServeUnixSocket(t *testing.T) {
	h := newTestHarness(t)
	socketPath := shortSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- h.server.Serve(ctx, socketPath, "", "", "", "")
	}()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}

	var resp *http.Response
	var err error
	var serveErr error
	var exited bool
	require.Eventually(t, func() bool {
		select {
		case serveErr = <-done:
			exited = true
			return true
		default:
		}
		resp, err = client.Get("http://unix/healthz")
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "server must become reachable over the unix socket")
	require.False(t, exited, "Serve returned before the socket became reachable: %v", serveErr)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	cancel()
	require.NoError(t, <-done)
}

func TestDashboardAssetsAreServed(t *testing.T) {
	h := newTestHarness(t)

	for _, path := range []string{"/dashboard/", "/dashboard/config"} {
		req, err := http.NewRequest(http.MethodGet, path, nil)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		h.server.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, path)
	}
}

func TestServeTCPRequiresBearerToken(t *testing.T) {
	h := newTestHarness(t)
	socketPath := shortSocketPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- h.server.Serve(ctx, socketPath, "127.0.0.1:18743", "secret-token", "", "")
	}()

	// exited records that Serve gave up on its own, in which case done is already
	// drained and the cleanup below must not read it again.
	var exited bool
	defer func() {
		cancel()
		if !exited {
			<-done
		}
	}()

	var unauthorized *http.Response
	var err error
	var serveErr error
	require.Eventually(t, func() bool {
		select {
		case serveErr = <-done:
			exited = true
			return true
		default:
		}
		unauthorized, err = http.Get("http://127.0.0.1:18743/healthz") //nolint:noctx // short-lived polling loop in a test
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "tcp listener must become reachable")
	require.False(t, exited, "Serve returned before the tcp listener became reachable: %v", serveErr)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode)
	_ = unauthorized.Body.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:18743/healthz", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret-token")
	authorized, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, authorized.StatusCode)
	_ = authorized.Body.Close()
}

func TestServeTCPWithTLS(t *testing.T) {
	h := newTestHarness(t)
	socketPath := shortSocketPath(t)
	certPath, keyPath := generateSelfSignedCert(t, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- h.server.Serve(ctx, socketPath, "127.0.0.1:18744", "secret-token", certPath, keyPath)
	}()

	var exited bool
	defer func() {
		cancel()
		if !exited {
			<-done
		}
	}()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test-only, self-signed cert

	var resp *http.Response
	var err error
	var serveErr error
	require.Eventually(t, func() bool {
		select {
		case serveErr = <-done:
			exited = true
			return true
		default:
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:18744/healthz", nil)
		require.NoError(t, reqErr)
		req.Header.Set("Authorization", "Bearer secret-token")
		resp, err = client.Do(req)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "tls tcp listener must become reachable")
	require.False(t, exited, "Serve returned before the tls listener became reachable: %v", serveErr)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
}

// TestSecurityHeadersOnEveryResponse checks a defensive header set is present
// on both an authenticated API route and the unauthenticated dashboard shell,
// since securityHeaders wraps the whole router rather than individual routes.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newTestHarness(t)

	for _, path := range []string{"/healthz", "/dashboard/"} {
		req, err := http.NewRequest(http.MethodGet, path, nil)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		h.server.ServeHTTP(rec, req)

		require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"), path)
		require.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"), path)
		require.Contains(t, rec.Header().Get("Content-Security-Policy"), "default-src 'self'", path)
	}
}
