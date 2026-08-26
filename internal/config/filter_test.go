// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ramify.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

const baseConfig = `
base_domain: preview.example.com
server:
  socket_path: /var/run/ramify/ramify.sock
store:
  path: /var/lib/ramify/ramify.db
git:
  provider: github
  token: t
  webhook_secret: s
deploy:
  provider: compose
  ssh_addr: host:22
  ssh_private_key_path: /etc/ramify/key
  compose_file: /srv/compose.yml
  dns_target: 203.0.113.10
  certificate_dir: /srv/ramify/certificates
dns:
  provider: cloudflare
  zone: preview.example.com
  api_token: tok
acme:
  email: ops@example.com
  ca_dir_url: https://acme-v02.api.letsencrypt.org/directory
`

// Omitting the block entirely must load and admit everything, or upgrading
// would change the behavior of every existing installation.
func TestFilterBlockIsOptional(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseConfig))
	require.NoError(t, err)
	require.False(t, cfg.Filter.PROnly)
	require.Empty(t, cfg.Filter.AllowBranches)
	require.Empty(t, cfg.Filter.DenyBranches)
	require.Empty(t, cfg.Filter.RequiredLabels)
	require.Zero(t, cfg.Filter.MaxConcurrentEnvs)
}

func TestFilterBlockParses(t *testing.T) {
	cfg, err := Load(writeConfig(t, baseConfig+`
filter:
  pr_only: true
  allow_branches: ["feat/**", "release/*"]
  deny_branches: ["dependabot/**"]
  required_labels: ["preview"]
  max_concurrent_envs: 25
`))
	require.NoError(t, err)
	require.True(t, cfg.Filter.PROnly)
	require.Equal(t, []string{"feat/**", "release/*"}, cfg.Filter.AllowBranches)
	require.Equal(t, []string{"dependabot/**"}, cfg.Filter.DenyBranches)
	require.Equal(t, []string{"preview"}, cfg.Filter.RequiredLabels)
	require.Equal(t, 25, cfg.Filter.MaxConcurrentEnvs)
}

// certificate_dir is the only route TLS material has to a Compose host, so an
// omission must surface at startup rather than as five failed applies later.
func TestComposeRequiresCertificateDir(t *testing.T) {
	body := strings.Replace(baseConfig, "  certificate_dir: /srv/ramify/certificates\n", "", 1)
	_, err := Load(writeConfig(t, body))
	require.Error(t, err)
	require.Contains(t, err.Error(), "deploy.certificate_dir")
}

// Kubernetes installs certificates as Secrets, so it must not inherit the
// requirement.
func TestKubernetesDoesNotRequireCertificateDir(t *testing.T) {
	body := strings.Replace(baseConfig, "  certificate_dir: /srv/ramify/certificates\n", "", 1)
	body = strings.Replace(body, "provider: compose", "provider: kubernetes\n  kubernetes_namespace: ramify", 1)
	_, err := Load(writeConfig(t, body))
	require.NoError(t, err)
}

// A negative ceiling is a typo, not a request for unlimited. Silently treating
// it as unlimited would be the opposite of what the operator asked for.
func TestNegativeMaxConcurrentEnvsIsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, baseConfig+`
filter:
  max_concurrent_envs: -1
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "max_concurrent_envs")
}
