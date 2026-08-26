// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
)

type fakeRunner struct {
	manifests []string
	calls     [][]string
}

func (f *fakeRunner) Run(_ context.Context, args []string, stdin string) (string, error) {
	f.calls = append(f.calls, args)
	if len(args) > 0 && args[0] == "apply" {
		f.manifests = append(f.manifests, stdin)
		return "configured", nil
	}
	if len(args) > 0 && args[0] == "get" {
		return "1", nil
	}
	return "", nil
}

func TestProviderContract(t *testing.T) {
	runner := &fakeRunner{}
	p := NewWithRunner(runner, "ramify", "preview.example.com", "203.0.113.10", "nginx", 8080, 80)
	contract.RunDeployProviderContract(t, p)
	require.Len(t, runner.manifests, 2)
	require.Contains(t, runner.manifests[0], "kind: Deployment")
	require.Contains(t, runner.manifests[0], "kind: Ingress")
	require.True(t, strings.Contains(runner.manifests[0], "contract-branch.preview.example.com"))
}

func TestManifestUsesArtifactRef(t *testing.T) {
	p := NewWithRunner(&fakeRunner{}, "ramify", "example.com", "127.0.0.1", "", 3000, 80)
	manifest := p.manifest("ramify-test", providerapi.EnvSpec{Subdomain: "feature", ArtifactRef: "registry.example/app:v1"})
	require.Contains(t, manifest, "image: \"registry.example/app:v1\"")
	require.Contains(t, manifest, "port: 80")
}

// TestIngressRoutesOnHostNotSecretName guards the ordering of manifest's format
// arguments: a swap between the TLS secretName and the rule host produces a
// manifest that still mentions the hostname but routes nothing to the Service.
func TestIngressRoutesOnHostNotSecretName(t *testing.T) {
	p := NewWithRunner(&fakeRunner{}, "ramify", "preview.example.com", "203.0.113.10", "nginx", 8080, 80)
	manifest := p.manifest("ramify-test", providerapi.EnvSpec{Subdomain: "feature"})

	host := "feature.preview.example.com"
	require.Contains(t, manifest, "  - host: "+strconv.Quote(host))
	require.Contains(t, manifest, "    secretName: "+tlsSecretName(host))
	require.NotContains(t, manifest, "    secretName: "+strconv.Quote(host))
	require.Contains(t, manifest, "    - "+strconv.Quote(host))
	require.Contains(t, manifest, "    ingressClassName: \"nginx\"")
}

func TestValidNameRejectsUnsafeRefs(t *testing.T) {
	require.True(t, validName(deploymentName("acme/api", "feature/login")))
	for _, ref := range []string{
		"",
		"Ramify-Upper",
		"ramify test",
		"ramify/../etc",
		"-leading-dash",
		strings.Repeat("a", maxNameLength+1),
	} {
		require.False(t, validName(ref), "%q must be rejected", ref)
	}
}

// TestApplyRejectsUnsafePreviousRef verifies Apply refuses a stored deploy_ref that
// is not a valid object name rather than interpolating it into a manifest.
func TestApplyRejectsUnsafePreviousRef(t *testing.T) {
	runner := &fakeRunner{}
	p := NewWithRunner(runner, "ramify", "example.com", "127.0.0.1", "", 8080, 80)

	_, err := p.Apply(context.Background(), providerapi.EnvSpec{
		Project: "acme/api", Branch: "main", Subdomain: "main", PreviousRef: "evil name --all",
	})
	require.ErrorIs(t, err, providerapi.ErrPermanent)
	require.Empty(t, runner.manifests, "no manifest may be applied for a rejected ref")
}

// TestDeploymentNameIsStableAndDistinct checks the two properties the name derivation
// exists for: the same environment always resolves to the same object, and two
// branches never collide.
func TestDeploymentNameIsStableAndDistinct(t *testing.T) {
	a := deploymentName("acme/api", "feature/login")
	require.Equal(t, a, deploymentName("acme/api", "feature/login"))
	require.NotEqual(t, a, deploymentName("acme/api", "feature/logout"))
	require.NotEqual(t, a, deploymentName("acme/web", "feature/login"))
	require.LessOrEqual(t, len(a), maxNameLength)
}

func TestSleepAndWakeScaleTheDeployment(t *testing.T) {
	runner := &fakeRunner{}
	p := NewWithRunner(runner, "ramify", "example.com", "127.0.0.1", "", 8080, 80)

	require.NoError(t, p.Sleep(context.Background(), "ramify-abc123"))
	require.NoError(t, p.Wake(context.Background(), "ramify-abc123"))

	require.Contains(t, runner.calls[0], "--replicas=0")
	require.Contains(t, runner.calls[1], "--replicas=1")
}

func TestHealthCheckReportsUnreadyWhenNoReplicas(t *testing.T) {
	p := NewWithRunner(&emptyRunner{}, "ramify", "example.com", "127.0.0.1", "", 8080, 80)
	status, err := p.HealthCheck(context.Background(), "ramify-abc123")
	require.NoError(t, err)
	require.False(t, status.Healthy)
}

type emptyRunner struct{}

func (*emptyRunner) Run(context.Context, []string, string) (string, error) { return "", nil }
