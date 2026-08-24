// SPDX-License-Identifier: Apache-2.0

// Package compose implements providerapi.DeployProvider by running `docker compose`
// over SSH on a host the operator already controls. Ramify never builds anything
// itself: Apply only ever passes an already-built image tag to `docker compose up`.
package compose

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/khanalsaroj/ramify/internal/core/domain"
	"github.com/khanalsaroj/ramify/providers/providerapi"
)

const maxProjectNameLength = 63

// commandRunner executes a single shell command on the remote deploy host and
// returns its combined stdout+stderr. Apply/Sleep/Wake/Destroy/HealthCheck are
// expressed against this narrow interface, rather than directly against an SSH
// client, so unit tests can substitute a fake instead of opening a real network
// connection.
type commandRunner interface {
	Run(ctx context.Context, command string) (string, error)
}

// Provider implements providerapi.DeployProvider by running `docker compose`
// commands over SSH.
type Provider struct {
	runner      commandRunner
	composeFile string
	// dnsTarget is the address DNS records should point to once a deployment is
	// applied. All environments on a given host share one address; routing to the
	// right container by hostname is the operator's reverse proxy's job, wired via
	// the compose file this provider drives — Ramify itself never inspects
	// container networking.
	dnsTarget string
}

var _ providerapi.DeployProvider = (*Provider)(nil)

// New constructs a Provider that connects to addr over SSH as user, authenticating
// with signer, running composeFile as the deploy target, and pointing DNS records
// at dnsTarget.
func New(addr, user string, signer ssh.Signer, hostKeyCallback ssh.HostKeyCallback, composeFile, dnsTarget string) *Provider {
	return &Provider{
		runner: &sshRunner{
			addr: addr,
			config: &ssh.ClientConfig{
				User:            user,
				Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
				HostKeyCallback: hostKeyCallback,
			},
		},
		composeFile: composeFile,
		dnsTarget:   dnsTarget,
	}
}

func newWithRunner(r commandRunner, composeFile, dnsTarget string) *Provider {
	return &Provider{runner: r, composeFile: composeFile, dnsTarget: dnsTarget}
}

// projectName derives a stable, idempotent docker compose project name from a
// project/branch pair, reusing the same DNS-label normalization used for
// subdomains since docker compose project names have a similarly restrictive
// character set.
func projectName(project, branch string) string {
	return "ramify-" + domain.Normalize(project+"-"+branch, maxProjectNameLength-len("ramify-"))
}

// Apply implements providerapi.DeployProvider. It is idempotent: calling it twice
// for the same Project/Branch runs `docker compose up -d` against the same project
// name both times, which docker compose itself treats as an update, not a new
// deployment.
func (p *Provider) Apply(ctx context.Context, spec providerapi.EnvSpec) (providerapi.Deployment, error) {
	name := projectName(spec.Project, spec.Branch)
	if spec.PreviousRef != "" {
		name = spec.PreviousRef
	}

	cmd := fmt.Sprintf(
		"IMAGE_TAG=%s COMPOSE_PROJECT_NAME=%s docker compose -f %s up -d",
		shellQuote(spec.ArtifactRef), shellQuote(name), shellQuote(p.composeFile),
	)
	if _, err := p.runner.Run(ctx, cmd); err != nil {
		return providerapi.Deployment{}, fmt.Errorf("compose: apply %s: %w", name, err)
	}
	return providerapi.Deployment{Ref: name, InternalAddr: p.dnsTarget}, nil
}

// Sleep implements providerapi.DeployProvider.
func (p *Provider) Sleep(ctx context.Context, ref string) error {
	cmd := fmt.Sprintf("docker compose -p %s -f %s stop", shellQuote(ref), shellQuote(p.composeFile))
	if _, err := p.runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("compose: sleep %s: %w", ref, err)
	}
	return nil
}

// Wake implements providerapi.DeployProvider.
func (p *Provider) Wake(ctx context.Context, ref string) error {
	cmd := fmt.Sprintf("docker compose -p %s -f %s start", shellQuote(ref), shellQuote(p.composeFile))
	if _, err := p.runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("compose: wake %s: %w", ref, err)
	}
	return nil
}

// Destroy implements providerapi.DeployProvider. `docker compose down` against a
// project with no running containers succeeds without error, so calling Destroy
// twice on the same ref is safe.
func (p *Provider) Destroy(ctx context.Context, ref string) error {
	cmd := fmt.Sprintf("docker compose -p %s -f %s down --volumes --remove-orphans", shellQuote(ref), shellQuote(p.composeFile))
	if _, err := p.runner.Run(ctx, cmd); err != nil {
		return fmt.Errorf("compose: destroy %s: %w", ref, err)
	}
	return nil
}

// HealthCheck implements providerapi.DeployProvider.
func (p *Provider) HealthCheck(ctx context.Context, ref string) (providerapi.Status, error) {
	cmd := fmt.Sprintf("docker compose -p %s -f %s ps --status running -q", shellQuote(ref), shellQuote(p.composeFile))
	out, err := p.runner.Run(ctx, cmd)
	if err != nil {
		return providerapi.Status{}, fmt.Errorf("compose: health check %s: %w", ref, err)
	}
	if strings.TrimSpace(out) == "" {
		return providerapi.Status{Healthy: false, Detail: "no running containers"}, nil
	}
	return providerapi.Status{Healthy: true, Detail: "running"}, nil
}

// shellQuote wraps s in single quotes for safe interpolation into a remote shell
// command, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
