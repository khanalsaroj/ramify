// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/khanalsaroj/ramify/internal/config"
)

func newInitCmd() *cobra.Command {
	var (
		output                  string
		baseDomain              string
		socketPath              string
		storePath               string
		reaperDefaultTTL        time.Duration
		eventRetention          time.Duration
		githubToken             string
		githubWebhookSecret     string
		gitProvider             string
		gitBaseURL              string
		deploySSHAddr           string
		deployProvider          string
		deploySSHUser           string
		deploySSHKeyPath        string
		deploySSHKnownHosts     string
		deployComposeFile       string
		deployDNSTarget         string
		deployCertificateDir    string
		kubernetesNamespace     string
		kubernetesContext       string
		kubernetesKubeconfig    string
		kubernetesIngressClass  string
		kubernetesContainerPort int
		kubernetesServicePort   int
		dnsZone                 string
		cloudflareAPIToken      string
		dnsProvider             string
		dnsAPIToken             string
		dnsProject              string
		dnsZoneID               string
		acmeEmail               string
		acmeCADirURL            string
		filterPROnly            bool
		filterAllowBranches     []string
		filterDenyBranches      []string
		filterRequiredLabels    []string
		filterMaxConcurrentEnvs int
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate ramify.yaml from flags",
		Long: "Generate ramify.yaml non-interactively from flags, so it can be scripted end to end " +
			"(see docs/quickstart.md). Secret-bearing flags accept a literal value or a \"$NAME\"/\"${NAME}\" " +
			"environment variable reference, written through to the file as given.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := config.Config{
				BaseDomain: baseDomain,
				Server:     config.ServerConfig{SocketPath: socketPath},
				Store:      config.StoreConfig{Path: storePath},
				Reaper:     config.ReaperConfig{Interval: 5 * time.Minute, DefaultTTL: reaperDefaultTTL, EventRetention: eventRetention},
				Git:        config.GitConfig{Provider: gitProvider, Token: githubToken, WebhookSecret: githubWebhookSecret, BaseURL: gitBaseURL},
				Deploy: config.DeployConfig{
					Provider: deployProvider,
					SSHAddr:  deploySSHAddr, SSHUser: deploySSHUser, SSHPrivateKeyPath: deploySSHKeyPath,
					SSHKnownHostsPath: deploySSHKnownHosts, ComposeFile: deployComposeFile, DNSTarget: deployDNSTarget, CertificateDir: deployCertificateDir,
					KubernetesNamespace: kubernetesNamespace, KubernetesContext: kubernetesContext, KubernetesKubeconfig: kubernetesKubeconfig,
					KubernetesIngressClass: kubernetesIngressClass, KubernetesContainerPort: kubernetesContainerPort, KubernetesServicePort: kubernetesServicePort,
				},
				DNS:  config.DNSConfig{Provider: dnsProvider, Zone: dnsZone, APIToken: firstNonEmpty(dnsAPIToken, cloudflareAPIToken), CloudflareAPIToken: cloudflareAPIToken, Project: dnsProject, ZoneID: dnsZoneID},
				ACME: config.ACMEConfig{Email: acmeEmail, CADirURL: acmeCADirURL},
				Filter: config.FilterConfig{
					PROnly: filterPROnly, AllowBranches: filterAllowBranches, DenyBranches: filterDenyBranches,
					RequiredLabels: filterRequiredLabels, MaxConcurrentEnvs: filterMaxConcurrentEnvs,
				},
			}

			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("generated config is incomplete: %w", err)
			}

			out, err := yaml.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("encoding config: %w", err)
			}
			if err := os.WriteFile(output, out, 0o600); err != nil {
				return fmt.Errorf("writing %s: %w", output, err)
			}

			printf(cmd.OutOrStdout(), "wrote %s\n", output)
			return nil
		},
	}

	cmd.Flags().StringVar(&output, "output", "/etc/ramify/ramify.yaml", "path to write the config file to")
	cmd.Flags().StringVar(&baseDomain, "base-domain", "", "DNS zone/suffix preview environments are published under (required)")
	cmd.Flags().StringVar(&socketPath, "socket-path", defaultSocketPath, "control API unix socket path")
	cmd.Flags().StringVar(&storePath, "store-path", "/var/lib/ramify/ramify.db", "state database path")
	cmd.Flags().DurationVar(&reaperDefaultTTL, "default-ttl", 72*time.Hour, "TTL applied to a new environment")
	cmd.Flags().DurationVar(&eventRetention, "event-retention", 720*time.Hour, "retention for completed event history")
	cmd.Flags().StringVar(&githubToken, "github-token", "", "GitHub token used to post PR comments")
	cmd.Flags().StringVar(&githubWebhookSecret, "github-webhook-secret", "", "GitHub webhook HMAC secret (required)")
	cmd.Flags().StringVar(&gitProvider, "git-provider", "github", "Git provider: github, gitlab, or bitbucket")
	cmd.Flags().StringVar(&gitBaseURL, "git-base-url", "", "Git provider base URL (for self-hosted GitLab or Bitbucket)")
	cmd.Flags().StringVar(&deploySSHAddr, "deploy-ssh-addr", "", "deploy host SSH address, host:port (required)")
	cmd.Flags().StringVar(&deployProvider, "deploy-provider", "compose", "deploy provider: compose or kubernetes")
	cmd.Flags().StringVar(&deploySSHUser, "deploy-ssh-user", "ramify", "deploy host SSH user")
	cmd.Flags().StringVar(&deploySSHKeyPath, "deploy-ssh-key", "", "path to the SSH private key used to reach the deploy host")
	cmd.Flags().StringVar(&deploySSHKnownHosts, "deploy-ssh-known-hosts", "", "path to a known_hosts file verifying the deploy host's key")
	cmd.Flags().StringVar(&deployComposeFile, "deploy-compose-file", "", "path to docker-compose.yml on the deploy host (required)")
	cmd.Flags().StringVar(&deployDNSTarget, "deploy-dns-target", "", "address DNS records should point to")
	cmd.Flags().StringVar(&deployCertificateDir, "deploy-certificate-dir", "/srv/ramify/certificates", "remote directory TLS material is installed into, read by your reverse proxy (required for compose)")
	cmd.Flags().StringVar(&kubernetesNamespace, "kubernetes-namespace", "ramify", "Kubernetes namespace for preview workloads")
	cmd.Flags().StringVar(&kubernetesContext, "kubernetes-context", "", "Kubernetes context to use")
	cmd.Flags().StringVar(&kubernetesKubeconfig, "kubernetes-kubeconfig", "", "path to Kubernetes kubeconfig")
	cmd.Flags().StringVar(&kubernetesIngressClass, "kubernetes-ingress-class", "", "Kubernetes Ingress class")
	cmd.Flags().IntVar(&kubernetesContainerPort, "kubernetes-container-port", 8080, "container port for Kubernetes workloads")
	cmd.Flags().IntVar(&kubernetesServicePort, "kubernetes-service-port", 8080, "Service port for Kubernetes workloads")
	cmd.Flags().StringVar(&dnsZone, "dns-zone", "", "DNS zone preview records are created in (required)")
	cmd.Flags().StringVar(&cloudflareAPIToken, "cloudflare-token", "", "Cloudflare API token")
	cmd.Flags().StringVar(&dnsProvider, "dns-provider", "cloudflare", "DNS provider: cloudflare, route53, googlecloud, or digitalocean")
	cmd.Flags().StringVar(&dnsAPIToken, "dns-token", "", "DNS API token (DigitalOcean; Cloudflare uses --cloudflare-token)")
	cmd.Flags().StringVar(&dnsProject, "dns-project", "", "Google Cloud project ID")
	cmd.Flags().StringVar(&dnsZoneID, "dns-zone-id", "", "Hosted/managed zone ID (Google Cloud or Route 53)")
	cmd.Flags().StringVar(&acmeEmail, "acme-email", "", "contact email for the ACME account (required)")
	cmd.Flags().StringVar(&acmeCADirURL, "acme-ca-dir-url", "https://acme-v02.api.letsencrypt.org/directory", "ACME directory URL")
	cmd.Flags().BoolVar(&filterPROnly, "pr-only", false, "only deploy branches that have an open pull request")
	cmd.Flags().StringSliceVar(&filterAllowBranches, "allow-branches", nil, "branch glob patterns to deploy; empty allows all (\"*\" does not cross a slash, \"**\" does)")
	cmd.Flags().StringSliceVar(&filterDenyBranches, "deny-branches", nil, "branch glob patterns to never deploy; evaluated before --allow-branches")
	cmd.Flags().StringSliceVar(&filterRequiredLabels, "required-labels", nil, "deploy a pull request only if it carries one of these labels (ignored where the host has no labels)")
	cmd.Flags().IntVar(&filterMaxConcurrentEnvs, "max-concurrent-envs", 0, "ceiling on live environments; 0 means unlimited")

	return cmd
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
