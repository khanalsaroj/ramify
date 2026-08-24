// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

type deployState int

const (
	deployStateRunning deployState = iota
	deployStateSleeping
	deployStateDestroyed
)

type deployment struct {
	spec  providerapi.EnvSpec
	state deployState
	// applyCount is incremented on every Apply call for this ref, so tests can
	// assert idempotency (no duplicate underlying resource created).
	applyCount int
}

// DeployProvider is an in-memory fake of providerapi.DeployProvider.
type DeployProvider struct {
	mu          sync.Mutex
	deployments map[string]*deployment // keyed by ref

	// ApplyErr, when set, is returned by every Apply call instead of succeeding.
	ApplyErr error
	// applyCalls counts every Apply invocation, including ones that fail, so
	// tests can assert retry/backoff behavior.
	applyCalls int
}

var _ providerapi.DeployProvider = (*DeployProvider)(nil)

// NewDeployProvider returns an empty fake DeployProvider.
func NewDeployProvider() *DeployProvider {
	return &DeployProvider{deployments: make(map[string]*deployment)}
}

func refFor(spec providerapi.EnvSpec) string {
	sum := sha256.Sum256([]byte(spec.Project + "/" + spec.Branch))
	return fmt.Sprintf("fake-deploy-%x", sum[:8])
}

// Apply implements providerapi.DeployProvider.
func (f *DeployProvider) Apply(_ context.Context, spec providerapi.EnvSpec) (providerapi.Deployment, error) {
	f.mu.Lock()
	f.applyCalls++
	f.mu.Unlock()
	if f.ApplyErr != nil {
		return providerapi.Deployment{}, f.ApplyErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	ref := spec.PreviousRef
	if ref == "" {
		ref = refFor(spec)
	}
	d, ok := f.deployments[ref]
	if !ok {
		d = &deployment{}
		f.deployments[ref] = d
	}
	d.spec = spec
	d.state = deployStateRunning
	d.applyCount++
	return providerapi.Deployment{Ref: ref, InternalAddr: "10.0.0.1:8080"}, nil
}

// Sleep implements providerapi.DeployProvider.
func (f *DeployProvider) Sleep(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deployments[ref]
	if !ok {
		return fmt.Errorf("fakes: unknown deployment ref %q", ref)
	}
	d.state = deployStateSleeping
	return nil
}

// Wake implements providerapi.DeployProvider.
func (f *DeployProvider) Wake(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deployments[ref]
	if !ok {
		return fmt.Errorf("fakes: unknown deployment ref %q", ref)
	}
	d.state = deployStateRunning
	return nil
}

// Destroy implements providerapi.DeployProvider. Destroying an already-destroyed or
// unknown ref is not an error, matching the idempotency requirement on teardown.
func (f *DeployProvider) Destroy(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deployments[ref]
	if !ok {
		return nil
	}
	d.state = deployStateDestroyed
	return nil
}

// HealthCheck implements providerapi.DeployProvider.
func (f *DeployProvider) HealthCheck(_ context.Context, ref string) (providerapi.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deployments[ref]
	if !ok {
		return providerapi.Status{}, fmt.Errorf("fakes: unknown deployment ref %q", ref)
	}
	switch d.state {
	case deployStateRunning:
		return providerapi.Status{Healthy: true, Detail: "running"}, nil
	case deployStateSleeping:
		return providerapi.Status{Healthy: false, Detail: "sleeping"}, nil
	default:
		return providerapi.Status{Healthy: false, Detail: "destroyed"}, nil
	}
}

// ApplyCount reports how many times Apply has succeeded for ref, so tests can
// assert idempotent behavior (no duplicate resource creation on repeated Apply).
func (f *DeployProvider) ApplyCount(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deployments[ref]
	if !ok {
		return 0
	}
	return d.applyCount
}

// ApplyCalls reports how many times Apply has been invoked in total, including
// calls that failed, so tests can assert retry/backoff behavior.
func (f *DeployProvider) ApplyCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyCalls
}
