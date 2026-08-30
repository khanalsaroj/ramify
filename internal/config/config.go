// SPDX-License-Identifier: Apache-2.0

// Package config loads and validates ramify.yaml, the control plane's single
// configuration file (see ramify.example.yaml at the repo root).
package config

import (
	"fmt"
	"log/slog"
	"net"
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
	Git        GitConfig    `yaml:"git"`
	Deploy     DeployConfig `yaml:"deploy"`
	DNS        DNSConfig    `yaml:"dns"`
	ACME       ACMEConfig   `yaml:"acme"`
	Filter     FilterConfig `yaml:"filter"`
	Notify     NotifyConfig `yaml:"notify"`
	Log        LogConfig    `yaml:"log"`
}

// ServerConfig configures the local control API listener (internal/api).
type ServerConfig struct {
	SocketPath string `yaml:"socket_path"`
	TCPAddr    string `yaml:"tcp_addr,omitempty"`
	TCPToken   string `yaml:"tcp_token,omitempty"`
	// TCPTLSCertFile and TCPTLSKeyFile, if both set, serve the TCP listener over
	// TLS instead of plaintext HTTP.
	TCPTLSCertFile string `yaml:"tcp_tls_cert_file,omitempty"`
	TCPTLSKeyFile  string `yaml:"tcp_tls_key_file,omitempty"`
	// TCPInsecureAllowRemote explicitly opts into binding tcp_addr to a
	// non-loopback address without TLS configured. Without it, a non-loopback
	// tcp_addr with no TLS cert/key is a startup error: the bearer token and
	// every environment API response would otherwise be sent in the clear to
	// whoever can observe the network path.
	TCPInsecureAllowRemote bool `yaml:"tcp_insecure_allow_remote,omitempty"`
}

// StoreConfig configures the SQLite state store.
type StoreConfig struct {
	Path string `yaml:"path"`
}

// ReaperConfig configures TTL-based environment expiry and the durable event
// worker.
type ReaperConfig struct {
	Interval       time.Duration `yaml:"interval"`
	DefaultTTL     time.Duration `yaml:"default_ttl"`
	EventRetention time.Duration `yaml:"event_retention"`
	// EventConcurrency caps how many due durable events (webhook/apply/destroy)
	// are processed at once. Events for the same project/branch always run one
	// at a time regardless of this value. Zero means use the built-in default
	// (8).
	EventConcurrency int `yaml:"event_concurrency,omitempty"`
}

// GitHubConfig configures providers/git/github.
type GitHubConfig struct {
	Token         string `yaml:"token"`
	WebhookSecret string `yaml:"webhook_secret"`
}

// GitConfig selects the Git hosting provider. The legacy github block remains
// supported so existing installations do not need to change immediately.
type GitConfig struct {
	Provider      string `yaml:"provider"`
	Token         string `yaml:"token"`
	WebhookSecret string `yaml:"webhook_secret"`
	BaseURL       string `yaml:"base_url,omitempty"`
}

// DeployConfig configures providers/deploy/compose.
type DeployConfig struct {
	Provider          string `yaml:"provider"`
	SSHAddr           string `yaml:"ssh_addr"`
	SSHUser           string `yaml:"ssh_user"`
	SSHPrivateKeyPath string `yaml:"ssh_private_key_path"`
	// SSHKnownHostsPath verifies the deploy host's key against an OpenSSH-format
	// known_hosts file. Required unless SSHInsecureSkipHostKeyVerify is set: with
	// neither, Ramify refuses to start rather than silently connecting to
	// whatever host answers on ssh_addr.
	SSHKnownHostsPath string `yaml:"ssh_known_hosts_path,omitempty"`
	// SSHInsecureSkipHostKeyVerify explicitly opts out of host key verification,
	// accepting any host that answers on ssh_addr. Only ever safe for a
	// throwaway/local test host; a network attacker who can intercept traffic to
	// ssh_addr can otherwise impersonate the deploy host and capture every
	// command Ramify runs.
	SSHInsecureSkipHostKeyVerify bool   `yaml:"ssh_insecure_skip_host_key_verify,omitempty"`
	ComposeFile                  string `yaml:"compose_file"`
	DNSTarget                    string `yaml:"dns_target"`
	// CertificateDir is the remote directory TLS material is installed into, read
	// by the operator's reverse proxy. Required for the compose provider: it is
	// the only path a certificate has to that host. Unused by kubernetes, which
	// installs certificates as Secrets instead.
	CertificateDir          string `yaml:"certificate_dir,omitempty"`
	KubernetesNamespace     string `yaml:"kubernetes_namespace,omitempty"`
	KubernetesContext       string `yaml:"kubernetes_context,omitempty"`
	KubernetesKubeconfig    string `yaml:"kubernetes_kubeconfig,omitempty"`
	KubernetesIngressClass  string `yaml:"kubernetes_ingress_class,omitempty"`
	KubernetesContainerPort int    `yaml:"kubernetes_container_port,omitempty"`
	KubernetesServicePort   int    `yaml:"kubernetes_service_port,omitempty"`
	// ReadinessTimeout and ReadinessPollInterval bound how long Apply waits for
	// the deploy provider's HealthCheck to report healthy before treating the
	// attempt as failed (it then retries with the reconciler's normal backoff).
	// Zero means use the built-in default (2m timeout, 2s poll interval).
	ReadinessTimeout      time.Duration `yaml:"readiness_timeout,omitempty"`
	ReadinessPollInterval time.Duration `yaml:"readiness_poll_interval,omitempty"`
}

// DNSConfig configures providers/dns/cloudflare.
type DNSConfig struct {
	Provider           string `yaml:"provider"`
	Zone               string `yaml:"zone"`
	CloudflareAPIToken string `yaml:"cloudflare_api_token"`
	APIToken           string `yaml:"api_token,omitempty"`
	Project            string `yaml:"project,omitempty"`
	ZoneID             string `yaml:"zone_id,omitempty"`
}

// ACMEConfig configures providers/cert/acme.
type ACMEConfig struct {
	Email      string `yaml:"email"`
	CADirURL   string `yaml:"ca_dir_url"`
	StorageDir string `yaml:"storage_dir"`
}

// FilterConfig decides which webhook events produce a preview environment.
// Its zero value admits everything, which is what Ramify did before the block
// existed: every rule here is opt-in, and omitting the block changes nothing.
type FilterConfig struct {
	// PROnly ignores branch pushes with no associated pull request.
	PROnly bool `yaml:"pr_only"`
	// AllowBranches and DenyBranches are glob patterns following the same
	// convention as GitHub Actions branch filters: "*" does not cross a slash,
	// "**" does. Deny is evaluated first and wins. An empty AllowBranches allows
	// every branch that Deny does not reject.
	AllowBranches []string `yaml:"allow_branches"`
	DenyBranches  []string `yaml:"deny_branches"`
	// RequiredLabels gates on pull request labels, matched case insensitively. A
	// request must carry at least one. The rule is skipped where the host cannot
	// report labels: Bitbucket Cloud has none, and a bare branch push has no
	// request to carry them. Pair with PROnly to make it absolute.
	RequiredLabels []string `yaml:"required_labels"`
	// MaxConcurrentEnvs caps live environments (every status except destroyed).
	// At the ceiling a push for a *new* environment is skipped; pushes to
	// environments that already exist still deploy. Zero means no ceiling.
	MaxConcurrentEnvs int `yaml:"max_concurrent_envs"`
}

// NotifyConfig configures providers/notify/prcomment. CommentTemplates maps a
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
var secretFields = []string{"git.token", "git.webhook_secret", "github.token", "github.webhook_secret", "dns.api_token", "dns.cloudflare_api_token"}

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

	if cfg.Git.Provider == "" {
		cfg.Git.Provider = "github"
	}
	if cfg.Git.Token == "" {
		cfg.Git.Token = cfg.GitHub.Token
	}
	if cfg.Git.WebhookSecret == "" {
		cfg.Git.WebhookSecret = cfg.GitHub.WebhookSecret
	}
	resolved, err := resolveEnv(cfg.Git.Token)
	if err != nil {
		return nil, fmt.Errorf("config: git.token: %w", err)
	}
	cfg.Git.Token = resolved

	resolved, err = resolveEnv(cfg.Git.WebhookSecret)
	if err != nil {
		return nil, fmt.Errorf("config: git.webhook_secret: %w", err)
	}
	cfg.Git.WebhookSecret = resolved
	cfg.GitHub.Token = cfg.Git.Token
	cfg.GitHub.WebhookSecret = cfg.Git.WebhookSecret

	if cfg.DNS.Provider == "" {
		cfg.DNS.Provider = "cloudflare"
	}
	if cfg.DNS.APIToken == "" {
		cfg.DNS.APIToken = cfg.DNS.CloudflareAPIToken
	}
	resolved, err = resolveEnv(cfg.DNS.APIToken)
	if err != nil {
		return nil, fmt.Errorf("config: dns.api_token: %w", err)
	}
	cfg.DNS.APIToken = resolved
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
	deployProvider := c.Deploy.Provider
	if deployProvider == "" {
		deployProvider = "compose"
	}
	gitProvider := c.Git.Provider
	gitToken := c.Git.Token
	gitSecret := c.Git.WebhookSecret
	if gitProvider == "" {
		gitProvider = "github"
	}
	if gitToken == "" {
		gitToken = c.GitHub.Token
	}
	if gitSecret == "" {
		gitSecret = c.GitHub.WebhookSecret
	}
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
	if c.Server.TCPAddr != "" {
		tlsConfigured := c.Server.TCPTLSCertFile != "" && c.Server.TCPTLSKeyFile != ""
		if !tlsConfigured && !c.Server.TCPInsecureAllowRemote && !isLoopbackAddr(c.Server.TCPAddr) {
			missing = append(missing, "server.tcp_tls_cert_file and server.tcp_tls_key_file (or a loopback server.tcp_addr, or explicit server.tcp_insecure_allow_remote: true)")
		}
	}
	if gitToken == "" {
		missing = append(missing, "git.token")
	}
	if gitSecret == "" {
		missing = append(missing, "git.webhook_secret")
	}
	if gitProvider != "github" && gitProvider != "gitlab" && gitProvider != "bitbucket" {
		missing = append(missing, "git.provider (github, gitlab, or bitbucket)")
	}
	if deployProvider == "compose" {
		if c.Deploy.SSHAddr == "" {
			missing = append(missing, "deploy.ssh_addr")
		}
		if c.Deploy.ComposeFile == "" {
			missing = append(missing, "deploy.compose_file")
		}
		if c.Deploy.SSHPrivateKeyPath == "" {
			missing = append(missing, "deploy.ssh_private_key_path")
		}
		// Required, not optional. SSH is the only route TLS material has to a
		// Compose deploy host, so without this the certificate Ramify just
		// obtained has nowhere to go and InstallCertificate fails every apply
		// after five retries. Failing at startup names the cause; failing per
		// apply does not.
		if c.Deploy.CertificateDir == "" {
			missing = append(missing, "deploy.certificate_dir")
		}
		// Required, not optional, unless the operator explicitly accepts the risk.
		// Leaving both unset means Ramify would connect to whatever host answers
		// on ssh_addr with no way to detect impersonation.
		if c.Deploy.SSHKnownHostsPath == "" && !c.Deploy.SSHInsecureSkipHostKeyVerify {
			missing = append(missing, "deploy.ssh_known_hosts_path (or explicit deploy.ssh_insecure_skip_host_key_verify: true)")
		}
	}
	if deployProvider != "compose" && deployProvider != "kubernetes" {
		missing = append(missing, "deploy.provider (compose or kubernetes)")
	}
	if c.Deploy.DNSTarget == "" {
		missing = append(missing, "deploy.dns_target")
	}
	if deployProvider == "kubernetes" && c.Deploy.KubernetesNamespace == "" {
		missing = append(missing, "deploy.kubernetes_namespace")
	}
	if c.DNS.Zone == "" {
		missing = append(missing, "dns.zone")
	}
	dnsProvider := c.DNS.Provider
	if dnsProvider == "" {
		dnsProvider = "cloudflare"
	}
	if dnsProvider != "googlecloud" && dnsProvider != "route53" && c.DNS.APIToken == "" && c.DNS.CloudflareAPIToken == "" {
		missing = append(missing, "dns.api_token")
	}
	if dnsProvider != "cloudflare" && dnsProvider != "route53" && dnsProvider != "googlecloud" && dnsProvider != "digitalocean" {
		missing = append(missing, "dns.provider (cloudflare, route53, googlecloud, or digitalocean)")
	}
	if dnsProvider == "googlecloud" && c.DNS.Project == "" {
		missing = append(missing, "dns.project")
	}
	if dnsProvider == "googlecloud" && c.DNS.ZoneID == "" {
		missing = append(missing, "dns.zone_id")
	}
	if c.ACME.Email == "" {
		missing = append(missing, "acme.email")
	}
	if c.ACME.CADirURL == "" {
		missing = append(missing, "acme.ca_dir_url")
	}
	if c.Filter.MaxConcurrentEnvs < 0 {
		missing = append(missing, "filter.max_concurrent_envs (must be zero for unlimited, or a positive count)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

// isLoopbackAddr reports whether addr — a "host:port" listen address — binds
// to a loopback interface, the one case where an unencrypted TCP control API
// listener doesn't need an explicit operator override.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
