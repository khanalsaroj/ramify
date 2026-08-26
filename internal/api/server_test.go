// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
		done <- h.server.Serve(ctx, socketPath, "", "")
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
		done <- h.server.Serve(ctx, socketPath, "127.0.0.1:18743", "secret-token")
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
