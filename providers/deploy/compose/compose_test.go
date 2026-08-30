// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
)

// fakeComposeHost simulates just enough of `docker compose`'s state machine to
// exercise Provider's logic without a real network or Docker daemon: `up -d`
// starts (or updates) a named project, `down` removes it, `stop`/`start` toggle a
// running flag, and `ps --status running -q` reports one fake container ID iff the
// project exists and is running.
type fakeComposeHost struct {
	mu       sync.Mutex
	projects map[string]bool // name -> running
	commands []string
}

func newFakeComposeHost() *fakeComposeHost {
	return &fakeComposeHost{projects: make(map[string]bool)}
}

func (h *fakeComposeHost) Run(_ context.Context, command string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, command)

	name := extractFlagValue(command, "-p")
	if name == "" {
		name = extractEnvValue(command, "COMPOSE_PROJECT_NAME")
	}

	switch {
	case strings.Contains(command, "up -d"):
		h.projects[name] = true
		return "", nil
	case strings.Contains(command, "down"):
		delete(h.projects, name)
		return "", nil
	case strings.Contains(command, "stop"):
		if _, ok := h.projects[name]; ok {
			h.projects[name] = false
		}
		return "", nil
	case strings.Contains(command, "start"):
		if _, ok := h.projects[name]; ok {
			h.projects[name] = true
		}
		return "", nil
	case strings.Contains(command, "ps --status running -q"):
		if h.projects[name] {
			return "fake-container-id\n", nil
		}
		return "", nil
	default:
		return "", fmt.Errorf("fakeComposeHost: unrecognized command: %s", command)
	}
}

func extractFlagValue(command, flag string) string {
	parts := strings.Fields(command)
	for i, p := range parts {
		if p == flag && i+1 < len(parts) {
			return strings.Trim(parts[i+1], "'")
		}
	}
	return ""
}

func extractEnvValue(command, key string) string {
	for p := range strings.FieldsSeq(command) {
		if v, ok := strings.CutPrefix(p, key+"="); ok {
			return strings.Trim(v, "'")
		}
	}
	return ""
}

func TestDeployProviderContract(t *testing.T) {
	host := newFakeComposeHost()
	p := newWithRunner(host, "/srv/ramify/docker-compose.yml", "203.0.113.10")
	contract.RunDeployProviderContract(t, p)
}

func TestApplyIsIdempotentAtProviderLevel(t *testing.T) {
	host := newFakeComposeHost()
	p := newWithRunner(host, "/srv/ramify/docker-compose.yml", "203.0.113.10")
	ctx := context.Background()

	dep1, err := p.Apply(ctx, envSpec("acme/web", "feature-x", "ref1", ""))
	require.NoError(t, err)

	dep2, err := p.Apply(ctx, envSpec("acme/web", "feature-x", "ref2", dep1.Ref))
	require.NoError(t, err)

	require.Equal(t, dep1.Ref, dep2.Ref)
	require.Len(t, host.projects, 1, "no duplicate compose project created on repeated Apply")
}

func TestSleepAndWake(t *testing.T) {
	host := newFakeComposeHost()
	p := newWithRunner(host, "/srv/ramify/docker-compose.yml", "203.0.113.10")
	ctx := context.Background()

	dep, err := p.Apply(ctx, envSpec("acme/web", "feature-x", "ref1", ""))
	require.NoError(t, err)

	require.NoError(t, p.Sleep(ctx, dep.Ref))
	status, err := p.HealthCheck(ctx, dep.Ref)
	require.NoError(t, err)
	require.False(t, status.Healthy)

	require.NoError(t, p.Wake(ctx, dep.Ref))
	status, err = p.HealthCheck(ctx, dep.Ref)
	require.NoError(t, err)
	require.True(t, status.Healthy)
}

func TestApplyPropagatesRunnerError(t *testing.T) {
	p := newWithRunner(errorRunner{}, "/srv/ramify/docker-compose.yml", "203.0.113.10")
	_, err := p.Apply(context.Background(), envSpec("acme/web", "feature-x", "ref1", ""))
	require.Error(t, err)
}

func TestProjectNameIsStableAndSanitized(t *testing.T) {
	a := projectName("acme/web", "Feature/Login")
	b := projectName("acme/web", "Feature/Login")
	require.Equal(t, a, b)
	require.LessOrEqual(t, len(a), maxProjectNameLength)
	require.NotContains(t, a, "/")
}

func TestShellQuoteEscapesEmbeddedQuotes(t *testing.T) {
	got := shellQuote(`it's a "test"`)
	require.Equal(t, `'it'"'"'s a "test"'`, got)
}

type errorRunner struct{}

func (errorRunner) Run(context.Context, string) (string, error) {
	return "", fmt.Errorf("connection refused")
}

// recordingRunner records every command it's asked to run and succeeds, for
// tests that only care what command was sent, not simulating compose state.
type recordingRunner struct {
	mu       sync.Mutex
	commands []string
}

func (r *recordingRunner) Run(_ context.Context, command string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	return "", nil
}

func TestRemoveCertificateDeletesFiles(t *testing.T) {
	runner := &recordingRunner{}
	p := newWithRunner(runner, "/srv/ramify/docker-compose.yml", "203.0.113.10")
	p.SetCertificateDir("/srv/ramify/certificates")

	require.NoError(t, p.RemoveCertificate(context.Background(), "feature-x.preview.example.com"))

	require.Len(t, runner.commands, 1)
	require.Contains(t, runner.commands[0], "rm -f")
	require.Contains(t, runner.commands[0], "/srv/ramify/certificates/")
}

func TestRemoveCertificateRequiresConfiguredDir(t *testing.T) {
	p := newWithRunner(&recordingRunner{}, "/srv/ramify/docker-compose.yml", "203.0.113.10")
	err := p.RemoveCertificate(context.Background(), "feature-x.preview.example.com")
	require.Error(t, err)
}

func envSpec(project, branch, artifactRef, previousRef string) providerapi.EnvSpec {
	return providerapi.EnvSpec{Project: project, Branch: branch, Subdomain: branch, ArtifactRef: artifactRef, PreviousRef: previousRef}
}
