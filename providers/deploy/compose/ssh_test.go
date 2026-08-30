// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// TestDialContextRespectsCancellation verifies dialContext returns promptly
// when ctx is canceled during the SSH handshake, rather than blocking until
// the handshake times out or succeeds on its own — the behavior the
// ssh.Dial-in-a-goroutine implementation this replaced could not provide.
func TestDialContextRespectsCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	// Accept connections but never write the SSH handshake preamble, so
	// ssh.NewClientConn blocks forever on its own — only ctx cancellation (via
	// closing the underlying conn) can unblock dialContext below.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { <-make(chan struct{}); _ = conn.Close() }()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = dialContext(ctx, listener.Addr().String(), &ssh.ClientConfig{
		User:            "ramify",
		Auth:            []ssh.AuthMethod{ssh.Password("unused")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test-only, no real host
	})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, 2*time.Second, "dialContext must return promptly on ctx cancellation, not hang on the handshake")
}

// TestDialContextFailsFastOnUnreachableHost verifies a connection refused (no
// listener at all) still surfaces as an error rather than hanging.
func TestDialContextFailsFastOnUnreachableHost(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close()) // nothing listens here now

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = dialContext(ctx, addr, &ssh.ClientConfig{
		User:            "ramify",
		Auth:            []ssh.AuthMethod{ssh.Password("unused")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test-only, no real host
	})
	require.Error(t, err)
}
