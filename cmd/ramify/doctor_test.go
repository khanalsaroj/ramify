// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/internal/config"
)

// checkCloudflareToken and checkSSHReachable require a live Cloudflare account and
// a live SSH host respectively, so they aren't exercised here — see
// providers/dns/cloudflare and providers/deploy/compose for the unit-tested logic
// each thin wrapper delegates to. checkACMEDirectoryReachable and
// checkGitHubWebhookSecret don't need live third-party infrastructure.

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

func TestCheckGitHubWebhookSecretTooShort(t *testing.T) {
	cfg := &config.Config{GitHub: config.GitHubConfig{WebhookSecret: "short"}}
	got := checkGitHubWebhookSecret(cfg)
	require.False(t, got.ok)
}

func TestCheckGitHubWebhookSecretOK(t *testing.T) {
	cfg := &config.Config{GitHub: config.GitHubConfig{WebhookSecret: "a-sufficiently-long-secret-value"}}
	got := checkGitHubWebhookSecret(cfg)
	require.True(t, got.ok)
}
