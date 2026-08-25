// SPDX-License-Identifier: Apache-2.0

// Package config loads and validates ramify.yaml, the control plane's single
// configuration file (see ramify.example.yaml at the repo root).
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of ramify.yaml.
type Config struct {
	BaseDomain string       `yaml:"base_domain"`
	Server     ServerConfig `yaml:"server"`
	Store      StoreConfig  `yaml:"store"`
	Reaper     ReaperConfig `yaml:"reaper"`
	GitHub     GitHubConfig `yaml:"github"`
	Deploy     DeployConfig `yaml:"deploy"`
	DNS        DNSConfig    `yaml:"dns"`
	ACME       ACMEConfig   `yaml:"acme"`
	Notify     NotifyConfig `yaml:"notify"`
	Log        LogConfig    `yaml:"log"`
}

// ServerConfig configures the local control API listener (internal/api).
type ServerConfig struct {
	SocketPath string `yaml:"socket_path"`
	TCPAddr    string `yaml:"tcp_addr,omitempty"`
	TCPToken   string `yaml:"tcp_token,omitempty"`
}

// StoreConfig configures the SQLite state store.
type StoreConfig struct {
	Path string `yaml:"path"`
}

// ReaperConfig configures TTL-based environment expiry.
type ReaperConfig struct {
	Interval       time.Duration `yaml:"interval"`
	DefaultTTL     time.Duration `yaml:"default_ttl"`
	EventRetention time.Duration `yaml:"event_retention"`
}

// GitHubConfig configures providers/git/github.
type GitHubConfig struct {
	Token         string `yaml:"token"`
	WebhookSecret string `yaml:"webhook_secret"`
}

// DeployConfig configures providers/deploy/compose.
type DeployConfig struct {
	SSHAddr           string `yaml:"ssh_addr"`
	SSHUser           string `yaml:"ssh_user"`
	SSHPrivateKeyPath string `yaml:"ssh_private_key_path"`
	// SSHKnownHostsPath, if set, verifies the deploy host's key against an
	// OpenSSH-format known_hosts file. Left empty, the host key is not verified —
	// acceptable only for a first connection to a host you've provisioned
	// yourself; ramify doctor warns if this is unset.
	SSHKnownHostsPath string `yaml:"ssh_known_hosts_path,omitempty"`
	ComposeFile       string `yaml:"compose_file"`
	DNSTarget         string `yaml:"dns_target"`
	CertificateDir    string `yaml:"certificate_dir,omitempty"`
}

// DNSConfig configures providers/dns/cloudflare.
type DNSConfig struct {
	Zone               string `yaml:"zone"`
	CloudflareAPIToken string `yaml:"cloudflare_api_token"`
}

// ACMEConfig configures providers/cert/acme.
type ACMEConfig struct {
	Email      string `yaml:"email"`
	CADirURL   string `yaml:"ca_dir_url"`
	StorageDir string `yaml:"storage_dir"`
}

// NotifyConfig configures providers/notify/githubcomment. CommentTemplates maps a
// providerapi.NotifyEvent.Kind ("ready", "updated", "failed", "expiring",
// "destroyed") to a Go text/template string executed against that NotifyEvent. Any
// kind not present here falls back to a built-in default template.
type NotifyConfig struct {
	CommentTemplates map[string]string `yaml:"comment_templates"`
}

// LogConfig configures log/slog output.
type LogConfig struct {
	// Format is "json" or "text". Empty means auto-detect: text on a TTY, json
	// otherwise, or the RAMIFY_LOG_FORMAT environment variable if set.
	Format string `yaml:"format"`
}

// secretFields lists the config field names (not values) that hold secrets, purely
// for logging: §6 requires that Ramify log secret *names*, never values.
var secretFields = []string{"github.token", "github.webhook_secret", "dns.cloudflare_api_token"}

// LogValue implements slog.LogValuer, redacting every field in secretFields so
// logging a Config never leaks a secret value, only which fields were configured.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_domain", c.BaseDomain),
		slog.String("store_path", c.Store.Path),
		slog.String("deploy_ssh_addr", c.Deploy.SSHAddr),
		slog.String("dns_zone", c.DNS.Zone),
		slog.Any("secret_fields_configured", secretFields),
	)
}

// Load reads, parses, and validates the YAML config at path. Fields documented as
// secrets (see secretFields) are resolved from the environment: a value of the form
// "$NAME" or "${NAME}" is replaced with the environment variable NAME, so the
// literal secret never needs to appear in the config file on disk.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied config location (CLI flag/default), not untrusted input
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}

	resolved, err := resolveEnv(cfg.GitHub.Token)
	if err != nil {
		return nil, fmt.Errorf("config: github.token: %w", err)
	}
	cfg.GitHub.Token = resolved

	resolved, err = resolveEnv(cfg.GitHub.WebhookSecret)
	if err != nil {
		return nil, fmt.Errorf("config: github.webhook_secret: %w", err)
	}
	cfg.GitHub.WebhookSecret = resolved

	resolved, err = resolveEnv(cfg.DNS.CloudflareAPIToken)
	if err != nil {
		return nil, fmt.Errorf("config: dns.cloudflare_api_token: %w", err)
	}
	cfg.DNS.CloudflareAPIToken = resolved

	resolved, err = resolveEnv(cfg.Server.TCPToken)
	if err != nil {
		return nil, fmt.Errorf("config: server.tcp_token: %w", err)
	}
	cfg.Server.TCPToken = resolved

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate reports whether every field Ramify requires at startup is present.
func (c Config) Validate() error {
	var missing []string
	if c.BaseDomain == "" {
		missing = append(missing, "base_domain")
	}
	if c.Store.Path == "" {
		missing = append(missing, "store.path")
	}
	if c.Server.SocketPath == "" {
		missing = append(missing, "server.socket_path")
	}
	if c.Server.TCPAddr != "" && c.Server.TCPToken == "" {
		missing = append(missing, "server.tcp_token (required when server.tcp_addr is set)")
	}
	if c.GitHub.Token == "" {
		missing = append(missing, "github.token")
	}
	if c.GitHub.WebhookSecret == "" {
		missing = append(missing, "github.webhook_secret")
	}
	if c.Deploy.SSHAddr == "" {
		missing = append(missing, "deploy.ssh_addr")
	}
	if c.Deploy.ComposeFile == "" {
		missing = append(missing, "deploy.compose_file")
	}
	if c.Deploy.SSHPrivateKeyPath == "" {
		missing = append(missing, "deploy.ssh_private_key_path")
	}
	if c.Deploy.DNSTarget == "" {
		missing = append(missing, "deploy.dns_target")
	}
	if c.DNS.Zone == "" {
		missing = append(missing, "dns.zone")
	}
	if c.DNS.CloudflareAPIToken == "" {
		missing = append(missing, "dns.cloudflare_api_token")
	}
	if c.ACME.Email == "" {
		missing = append(missing, "acme.email")
	}
	if c.ACME.CADirURL == "" {
		missing = append(missing, "acme.ca_dir_url")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

// resolveEnv expands a "$NAME" or "${NAME}" value to the environment variable
// NAME. A value not starting with "$" is returned unchanged, so literal
// (non-secret) values remain usable in the config file.
func resolveEnv(value string) (string, error) {
	if value == "" || !strings.HasPrefix(value, "$") {
		return value, nil
	}
	name := strings.TrimPrefix(value, "$")
	name = strings.TrimPrefix(name, "{")
	name = strings.TrimSuffix(name, "}")

	resolved, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return resolved, nil
}
