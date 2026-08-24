// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/khanalsaroj/ramify/internal/core"
	"github.com/khanalsaroj/ramify/internal/store"
	"github.com/khanalsaroj/ramify/test/fakes"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func TestDeployHostKeyCallbackFallsBackToInsecure(t *testing.T) {
	cb, err := deployHostKeyCallback("", discardLogger())
	require.NoError(t, err)
	require.NotNil(t, cb)
}

func TestDeployHostKeyCallbackLoadsKnownHosts(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{"example.com:22"}, sshPub) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(line), 0o600))

	cb, err := deployHostKeyCallback(path, discardLogger())
	require.NoError(t, err)
	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}
	require.NoError(t, cb("example.com:22", addr, sshPub))
}

func TestDeployHostKeyCallbackMissingFile(t *testing.T) {
	_, err := deployHostKeyCallback(filepath.Join(t.TempDir(), "does-not-exist"), discardLogger())
	require.Error(t, err)
}

func TestRunReaperLoopStopsOnContextCancel(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	reconciler := core.NewReconciler(st, fakes.NewDeployProvider(), fakes.NewDNSProvider(),
		fakes.NewCertificateProvider(), fakes.NewNotifierProvider(), core.NewRealClock(), "preview.example.com", 0, discardLogger())
	reaper := core.NewReaper(st, reconciler, core.NewRealClock(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReaperLoop(ctx, reaper, 10*time.Millisecond, discardLogger())
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runReaperLoop did not stop after context cancellation")
	}
}

func TestNewLoggerRespectsEnvVar(t *testing.T) {
	t.Setenv("RAMIFY_LOG_FORMAT", "text")
	logger := newLogger()
	require.NotNil(t, logger)
}
