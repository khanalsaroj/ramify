// SPDX-License-Identifier: Apache-2.0

package providerapi

import "context"

// EnvSpec describes the desired state of a preview environment for a DeployProvider
// Apply call.
type EnvSpec struct {
	Project     string
	Branch      string
	Subdomain   string
	ArtifactRef string
	PreviousRef string // deploy_ref from prior Apply, empty on first create
}

// Deployment is the result of a successful DeployProvider.Apply call.
type Deployment struct {
	Ref          string // opaque provider handle, stored as deploy_ref
	InternalAddr string // where DNS should point
}

// Status is the result of a DeployProvider.HealthCheck call.
type Status struct {
	Healthy bool
	Detail  string
}

// DeployProvider creates, updates, sleeps, wakes, and destroys the compute backing a
// preview environment. Apply must be idempotent: calling it twice with the same
// EnvSpec must not create a duplicate deployment.
type DeployProvider interface {
	// Apply creates the environment on first call, or updates it in place on
	// subsequent calls with the same Project/Branch. It never receives a build
	// script, Dockerfile, or repository checkout — only an already-built
	// ArtifactRef.
	Apply(ctx context.Context, spec EnvSpec) (Deployment, error)
	// Sleep stops the deployment's compute without destroying it.
	Sleep(ctx context.Context, ref string) error
	// Wake restarts a deployment previously stopped by Sleep.
	Wake(ctx context.Context, ref string) error
	// Destroy removes the deployment's compute. Calling Destroy twice on the same
	// ref must not error.
	Destroy(ctx context.Context, ref string) error
	// HealthCheck reports whether the deployment identified by ref is healthy.
	HealthCheck(ctx context.Context, ref string) (Status, error)
}
