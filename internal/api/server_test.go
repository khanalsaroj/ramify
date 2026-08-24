// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServeUnixSocket(t *testing.T) {
	h := newTestHarness(t)
	socketPath := filepath.Join(t.TempDir(), "ramify.sock")

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
	require.Eventually(t, func() bool {
		resp, err = client.Get("http://unix/healthz")
		return err == nil
	}, 2*time.Second, 20*time.Millisecond, "server must become reachable over the unix socket")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	cancel()
	require.NoError(t, <-done)
}

func TestServeTCPRequiresBearerToken(t *testing.T) {
	h := newTestHarness(t)
	socketPath := filepath.Join(t.TempDir(), "ramify.sock")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- h.server.Serve(ctx, socketPath, "127.0.0.1:18743", "secret-token")
	}()
	defer func() {
		cancel()
		<-done
	}()

	var unauthorized *http.Response
	var err error
	require.Eventually(t, func() bool {
		unauthorized, err = http.Get("http://127.0.0.1:18743/healthz") //nolint:noctx // short-lived polling loop in a test
		return err == nil
	}, 2*time.Second, 20*time.Millisecond, "tcp listener must become reachable")
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
