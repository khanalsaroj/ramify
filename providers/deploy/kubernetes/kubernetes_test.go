package kubernetes

import (
	"context"
	"strings"
	"testing"

	"github.com/khanalsaroj/ramify/providers/providerapi"
	"github.com/khanalsaroj/ramify/test/contract"
	"github.com/stretchr/testify/require"
)

type fakeRunner struct{ manifests []string }

func (f *fakeRunner) Run(_ context.Context, args []string, stdin string) (string, error) {
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
