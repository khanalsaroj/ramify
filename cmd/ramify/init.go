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
		output               string
		baseDomain           string
		socketPath           string
		storePath            string
		reaperDefaultTTL     time.Duration
		eventRetention       time.Duration
		githubToken          string
		githubWebhookSecret  string
		deploySSHAddr        string
		deploySSHUser        string
		deploySSHKeyPath     string
		deploySSHKnownHosts  string
		deployComposeFile    string
		deployDNSTarget      string
		deployCertificateDir string
		dnsZone              string
		cloudflareAPIToken   string
		acmeEmail            string
		acmeCADirURL         string
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
				GitHub:     config.GitHubConfig{Token: githubToken, WebhookSecret: githubWebhookSecret},
				Deploy: config.DeployConfig{
					SSHAddr: deploySSHAddr, SSHUser: deploySSHUser, SSHPrivateKeyPath: deploySSHKeyPath,
					SSHKnownHostsPath: deploySSHKnownHosts, ComposeFile: deployComposeFile, DNSTarget: deployDNSTarget, CertificateDir: deployCertificateDir,
				},
				DNS:  config.DNSConfig{Zone: dnsZone, CloudflareAPIToken: cloudflareAPIToken},
				ACME: config.ACMEConfig{Email: acmeEmail, CADirURL: acmeCADirURL},
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
	cmd.Flags().StringVar(&deploySSHAddr, "deploy-ssh-addr", "", "deploy host SSH address, host:port (required)")
	cmd.Flags().StringVar(&deploySSHUser, "deploy-ssh-user", "ramify", "deploy host SSH user")
	cmd.Flags().StringVar(&deploySSHKeyPath, "deploy-ssh-key", "", "path to the SSH private key used to reach the deploy host")
	cmd.Flags().StringVar(&deploySSHKnownHosts, "deploy-ssh-known-hosts", "", "path to a known_hosts file verifying the deploy host's key")
	cmd.Flags().StringVar(&deployComposeFile, "deploy-compose-file", "", "path to docker-compose.yml on the deploy host (required)")
	cmd.Flags().StringVar(&deployDNSTarget, "deploy-dns-target", "", "address DNS records should point to")
	cmd.Flags().StringVar(&deployCertificateDir, "deploy-certificate-dir", "", "remote directory for installed TLS certificate material")
	cmd.Flags().StringVar(&dnsZone, "dns-zone", "", "Cloudflare DNS zone (required)")
	cmd.Flags().StringVar(&cloudflareAPIToken, "cloudflare-token", "", "Cloudflare API token")
	cmd.Flags().StringVar(&acmeEmail, "acme-email", "", "contact email for the ACME account (required)")
	cmd.Flags().StringVar(&acmeCADirURL, "acme-ca-dir-url", "https://acme-v02.api.letsencrypt.org/directory", "ACME directory URL")

	return cmd
}
