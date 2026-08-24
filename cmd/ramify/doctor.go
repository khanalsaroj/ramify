// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/khanalsaroj/ramify/internal/config"
)

type doctorCheck struct {
	name   string
	detail string
	ok     bool
}

func newDoctorCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate the local config and connectivity to every configured provider",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				printf(cmd.OutOrStdout(), "[FAIL] config file valid: %v\n", err)
				return fmt.Errorf("doctor: config invalid")
			}
			printLine(cmd.OutOrStdout(), "[ OK ] config file valid")

			checks := []doctorCheck{
				checkCloudflareToken(cfg),
				checkSSHReachable(cfg),
				checkGitHubWebhookSecret(cfg),
				checkACMEDirectoryReachable(cfg),
			}

			failed := false
			for _, c := range checks {
				status := "OK"
				if !c.ok {
					status = "FAIL"
					failed = true
				}
				printf(cmd.OutOrStdout(), "[%4s] %s: %s\n", status, c.name, c.detail)
			}
			if failed {
				return fmt.Errorf("doctor: one or more checks failed")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "ramify.yaml", "path to ramify.yaml")
	return cmd
}

// checkCloudflareToken verifies the configured token can at least resolve the
// configured zone. It cannot independently confirm zone-edit scope without making
// a mutating API call, which doctor deliberately avoids.
func checkCloudflareToken(cfg *config.Config) doctorCheck {
	name := "Cloudflare token resolves configured zone"
	api, err := cf.NewWithAPIToken(cfg.DNS.CloudflareAPIToken)
	if err != nil {
		return doctorCheck{name: name, ok: false, detail: err.Error()}
	}
	zoneID, err := api.ZoneIDByName(cfg.DNS.Zone)
	if err != nil {
		return doctorCheck{name: name, ok: false, detail: err.Error()}
	}
	return doctorCheck{name: name, ok: true, detail: "zone ID " + zoneID}
}

// checkSSHReachable dials the deploy host and authenticates. It does not verify the
// host key against ssh_known_hosts_path (that verification only matters for the
// real deploy provider's traffic); a first-run doctor check on a host you just
// provisioned has no prior trust anchor to check against.
func checkSSHReachable(cfg *config.Config) doctorCheck {
	name := "SSH deploy host reachable and authorized"
	keyBytes, err := os.ReadFile(cfg.Deploy.SSHPrivateKeyPath)
	if err != nil {
		return doctorCheck{name: name, ok: false, detail: err.Error()}
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return doctorCheck{name: name, ok: false, detail: err.Error()}
	}

	clientConfig := &ssh.ClientConfig{
		User:            cfg.Deploy.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // doctor connectivity check only; the real deploy provider uses ssh_known_hosts_path
		Timeout:         10 * time.Second,
	}

	sshClient, err := ssh.Dial("tcp", cfg.Deploy.SSHAddr, clientConfig)
	if err != nil {
		return doctorCheck{name: name, ok: false, detail: err.Error()}
	}
	defer func() { _ = sshClient.Close() }() // best-effort; check result already captured

	if cfg.Deploy.SSHKnownHostsPath == "" {
		return doctorCheck{name: name, ok: true, detail: "connected, but ssh_known_hosts_path is unset — host key is not verified in production"}
	}
	return doctorCheck{name: name, ok: true, detail: "connected and authenticated"}
}

func checkGitHubWebhookSecret(cfg *config.Config) doctorCheck {
	name := "GitHub webhook secret configured"
	if len(cfg.GitHub.WebhookSecret) < 16 {
		return doctorCheck{name: name, ok: false, detail: "secret is set but shorter than 16 characters"}
	}
	return doctorCheck{name: name, ok: true, detail: "configured"}
}

// checkACMEDirectoryReachable checks that the configured ACME CA directory URL
// responds, without registering an account: New() generates a fresh account key
// and registers a new account on every call, so doctor deliberately doesn't invoke
// it — that would leave behind a new, unused ACME account on every run.
func checkACMEDirectoryReachable(cfg *config.Config) doctorCheck {
	name := "ACME directory reachable"
	httpClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
	}
	resp, err := httpClient.Get(cfg.ACME.CADirURL) //nolint:gosec,noctx // operator-supplied, config-driven URL, one-shot doctor check
	if err != nil {
		return doctorCheck{name: name, ok: false, detail: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }() // response not needed beyond the status code
	if resp.StatusCode >= 300 {
		return doctorCheck{name: name, ok: false, detail: fmt.Sprintf("unexpected status %d", resp.StatusCode)}
	}
	return doctorCheck{name: name, ok: true, detail: "reachable"}
}
