// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/internal/config"
)

// checkCloudflareToken, checkSSHReachable, and checkKubernetesReachable require a
// live Cloudflare account, SSH host, and cluster respectively, so they aren't
// exercised here — see providers/dns/cloudflare, providers/deploy/compose, and
// providers/deploy/kubernetes for the unit-tested logic each thin wrapper
// delegates to. checkACMEDirectoryReachable, checkWebhookSecret, and
// checkDeployProvider's dispatch don't need live third-party infrastructure.

func TestCheckACMEDirectoryReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{ACME: config.ACMEConfig{CADirURL: srv.URL}}
	got := checkACMEDirectoryReachable(cfg)
	require.True(t, got.ok)
}

func TestCheckACMEDirectoryUnreachable(t *testing.T) {
	cfg := &config.Config{ACME: config.ACMEConfig{CADirURL: "http://127.0.0.1:1"}}
	got := checkACMEDirectoryReachable(cfg)
	require.False(t, got.ok)
}

func TestCheckWebhookSecretTooShort(t *testing.T) {
	cfg := &config.Config{Git: config.GitConfig{WebhookSecret: "short"}}
	got := checkWebhookSecret(cfg)
	require.False(t, got.ok)
}

func TestCheckWebhookSecretOK(t *testing.T) {
	cfg := &config.Config{Git: config.GitConfig{WebhookSecret: "a-sufficiently-long-secret-value"}}
	got := checkWebhookSecret(cfg)
	require.True(t, got.ok)
}

// A gitlab or bitbucket config carries the same secret in the same field, so the
// check must not depend on the provider name.
func TestCheckWebhookSecretIsProviderIndependent(t *testing.T) {
	for _, provider := range []string{"github", "gitlab", "bitbucket"} {
		cfg := &config.Config{Git: config.GitConfig{Provider: provider, WebhookSecret: "a-sufficiently-long-secret-value"}}
		require.True(t, checkWebhookSecret(cfg).ok, provider)
	}
}

// A kubernetes deploy target has no ssh_private_key_path, so dispatching to the
// compose SSH check would fail every valid kubernetes config. Guard the routing
// itself; the cluster call behind it needs a live cluster.
func TestCheckDeployProviderRejectsUnknownProvider(t *testing.T) {
	cfg := &config.Config{Deploy: config.DeployConfig{Provider: "nomad"}}
	got := checkDeployProvider(context.Background(), cfg)
	require.False(t, got.ok)
	require.Contains(t, got.detail, "nomad")
}

// An empty deploy.provider means compose, matching config.Validate's default.
func TestCheckDeployProviderDefaultsToCompose(t *testing.T) {
	cfg := &config.Config{Deploy: config.DeployConfig{SSHPrivateKeyPath: filepath.Join(t.TempDir(), "absent")}}
	got := checkDeployProvider(context.Background(), cfg)
	require.False(t, got.ok)
	require.Equal(t, "SSH deploy host reachable and authorized", got.name)
}
