// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const validYAML = `
base_domain: preview.example.com
store:
  path: /var/lib/ramify/ramify.db
github:
  webhook_secret: $TEST_WEBHOOK_SECRET
deploy:
  ssh_addr: deploy.example.com:22
  compose_file: /srv/ramify/docker-compose.yml
dns:
  zone: preview.example.com
acme:
  email: ops@example.com
`

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ramify.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestLoadValidConfig(t *testing.T) {
	t.Setenv("TEST_WEBHOOK_SECRET", "shh-its-a-secret")
	path := writeTempConfig(t, validYAML)

	cfg, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "preview.example.com", cfg.BaseDomain)
	require.Equal(t, "shh-its-a-secret", cfg.GitHub.WebhookSecret, "$NAME must resolve from the environment")
}

func TestLoadMissingEnvVar(t *testing.T) {
	path := writeTempConfig(t, validYAML)
	_, err := Load(path)
	require.Error(t, err)
}

func TestLoadMissingRequiredFields(t *testing.T) {
	path := writeTempConfig(t, "base_domain: preview.example.com\n")
	_, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "store.path")
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Error(t, err)
}

func TestResolveEnvLiteralValuePassesThrough(t *testing.T) {
	got, err := resolveEnv("literal-value")
	require.NoError(t, err)
	require.Equal(t, "literal-value", got)
}

func TestResolveEnvBracedForm(t *testing.T) {
	t.Setenv("TEST_BRACED_VAR", "braced-value")
	got, err := resolveEnv("${TEST_BRACED_VAR}")
	require.NoError(t, err)
	require.Equal(t, "braced-value", got)
}

func TestLogValueRedactsSecrets(t *testing.T) {
	cfg := Config{
		BaseDomain: "preview.example.com",
		GitHub:     GitHubConfig{Token: "super-secret-token", WebhookSecret: "another-secret"},
	}
	logged := cfg.LogValue().String()
	require.NotContains(t, logged, "super-secret-token")
	require.NotContains(t, logged, "another-secret")
}
