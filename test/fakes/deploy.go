// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"

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
	// ApplyErrOnRef, when set, is returned only by an Apply call whose
	// ArtifactRef matches a key in the map, letting a test make one specific
	// revision fail while another (e.g. a rollback target) keeps succeeding.
	ApplyErrOnRef map[string]error
	// SleepErr and WakeErr, when set, are returned by every Sleep/Wake call
	// instead of succeeding.
	SleepErr error
	WakeErr  error
	// DestroyErr, when set, is returned by every Destroy call instead of
	// succeeding, so tests can exercise compensating-cleanup ordering when one
	// teardown step fails.
	DestroyErr error
	// applyCalls counts every Apply invocation, including ones that fail, so
	// tests can assert retry/backoff behavior.
	applyCalls int
	// UnhealthyChecks, when non-zero, makes the first UnhealthyChecks
	// HealthCheck calls per ref report unhealthy before reporting healthy, so
	// tests can exercise Apply's readiness-polling loop deterministically.
	UnhealthyChecks int
	healthChecks    map[string]int // ref -> HealthCheck calls so far

	// removedCertificates records every RemoveCertificate call, in order, so
	// tests can assert Destroy tears down deployed TLS material.
	removedCertificates []string

	// ApplyGate, if non-nil, makes every Apply call block on a receive from this
	// channel before proceeding, letting a test control exactly how many Apply
	// calls are in flight at once — used to assert bounded concurrency.
	ApplyGate   chan struct{}
	inFlight    int32
	maxInFlight int32
}

var _ providerapi.DeployProvider = (*DeployProvider)(nil)
var _ providerapi.CertificateRemover = (*DeployProvider)(nil)

// NewDeployProvider returns an empty fake DeployProvider.
func NewDeployProvider() *DeployProvider {
	return &DeployProvider{deployments: make(map[string]*deployment), healthChecks: make(map[string]int)}
}

func refFor(spec providerapi.EnvSpec) string {
	sum := sha256.Sum256([]byte(spec.Project + "/" + spec.Branch))
	return fmt.Sprintf("fake-deploy-%x", sum[:8])
}

// Apply implements providerapi.DeployProvider.
func (f *DeployProvider) Apply(_ context.Context, spec providerapi.EnvSpec) (providerapi.Deployment, error) {
	f.mu.Lock()
	f.applyCalls++
	gate := f.ApplyGate
	refErr := f.ApplyErrOnRef[spec.ArtifactRef]
	f.mu.Unlock()
	if f.ApplyErr != nil {
		return providerapi.Deployment{}, f.ApplyErr
	}
	if refErr != nil {
		return providerapi.Deployment{}, refErr
	}
	if gate != nil {
		n := atomic.AddInt32(&f.inFlight, 1)
		for {
			old := atomic.LoadInt32(&f.maxInFlight)
			if n <= old {
				break
			}
			if atomic.CompareAndSwapInt32(&f.maxInFlight, old, n) {
				break
			}
		}
		<-gate
		atomic.AddInt32(&f.inFlight, -1)
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
	if f.SleepErr != nil {
		return f.SleepErr
	}
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
	if f.WakeErr != nil {
		return f.WakeErr
	}
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
	if f.DestroyErr != nil {
		return f.DestroyErr
	}
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
	if d.state == deployStateRunning && f.healthChecks[ref] < f.UnhealthyChecks {
		f.healthChecks[ref]++
		return providerapi.Status{Healthy: false, Detail: "starting"}, nil
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

// RemoveCertificate implements providerapi.CertificateRemover.
func (f *DeployProvider) RemoveCertificate(_ context.Context, domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedCertificates = append(f.removedCertificates, domain)
	return nil
}

// RemovedCertificates reports every domain RemoveCertificate has been called
// with, in call order.
func (f *DeployProvider) RemovedCertificates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removedCertificates...)
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

// MaxInFlight reports the highest number of concurrent Apply calls observed
// while ApplyGate was set, so tests can assert a bounded worker pool never
// exceeded its configured ceiling.
func (f *DeployProvider) MaxInFlight() int32 {
	return atomic.LoadInt32(&f.maxInFlight)
}
