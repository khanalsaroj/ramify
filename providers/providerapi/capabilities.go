// SPDX-License-Identifier: Apache-2.0

package providerapi

import "context"

// CertificateInstaller is an optional DeployProvider capability for deploy
// targets that need TLS material pushed to them directly — for example Compose
// behind a reverse proxy that reads certificate files from disk, or Kubernetes
// writing a TLS Secret an Ingress already references. Not every DeployProvider
// needs it, so it is checked via a type assertion rather than added to
// DeployProvider itself.
type CertificateInstaller interface {
	InstallCertificate(ctx context.Context, domain string, certificatePEM, privateKeyPEM []byte) error
}

// CertificateRemover is the matching teardown capability: removing whatever
// InstallCertificate wrote for domain. Called during Destroy so private key
// material does not outlive the environment it was issued for.
type CertificateRemover interface {
	RemoveCertificate(ctx context.Context, domain string) error
}

// LogFetcher is an optional DeployProvider capability for retrieving the tail
// of a deployment's logs, used by `ramify logs`. Like CertificateInstaller, it
// has no meaningful contract shared across every possible deploy target, so it
// is not part of DeployProvider.
type LogFetcher interface {
	Logs(ctx context.Context, ref string) (string, error)
}
