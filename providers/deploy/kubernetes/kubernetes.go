// SPDX-License-Identifier: Apache-2.0

// Package kubernetes implements providerapi.DeployProvider by shelling out to
// kubectl, materializing each preview environment as a Deployment, a Service, and
// an Ingress inside one namespace.
//
// kubectl is used rather than client-go deliberately: it keeps authentication,
// cluster selection, and credential plugins (EKS, GKE, OIDC) as the operator
// already configured them in kubeconfig, instead of reimplementing that chain.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

// maxNameLength is the RFC 1123 label limit Kubernetes enforces on object names.
// Every name this package generates or accepts is validated against it.
const maxNameLength = 63

// namePattern is the RFC 1123 subdomain form Kubernetes requires of Deployment,
// Service, and Ingress names.
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type commandRunner interface {
	Run(ctx context.Context, args []string, stdin string) (string, error)
}

// Provider manages one Kubernetes namespace and uses dnsTarget as the address of
// the cluster ingress or load balancer. Ingress rules route each preview host to
// its per-environment Service.
type Provider struct {
	runner        commandRunner
	namespace     string
	baseDomain    string
	dnsTarget     string
	ingressClass  string
	containerPort int
	servicePort   int
}

var _ providerapi.DeployProvider = (*Provider)(nil)
var _ providerapi.CertificateInstaller = (*Provider)(nil)
var _ providerapi.CertificateRemover = (*Provider)(nil)
var _ providerapi.LogFetcher = (*Provider)(nil)

// New constructs a Provider backed by kubectl. Authentication and cluster
// selection are delegated to kubeconfig, the active context, and standard kubectl
// environment handling.
func New(namespace, baseDomain, dnsTarget, ingressClass, kubeconfig, kubeContext string, containerPort, servicePort int) *Provider {
	return NewWithRunner(&execRunner{kubeconfig: kubeconfig, kubeContext: kubeContext},
		namespace, baseDomain, dnsTarget, ingressClass, containerPort, servicePort)
}

// NewWithRunner constructs a Provider over an arbitrary command runner. It exists
// so tests can capture the manifests that would be applied without a cluster.
func NewWithRunner(r commandRunner, namespace, baseDomain, dnsTarget, ingressClass string, containerPort, servicePort int) *Provider {
	if namespace == "" {
		namespace = "default"
	}
	if containerPort == 0 {
		containerPort = 8080
	}
	if servicePort == 0 {
		servicePort = containerPort
	}
	return &Provider{
		runner:        r,
		namespace:     namespace,
		baseDomain:    strings.TrimSuffix(baseDomain, "."),
		dnsTarget:     dnsTarget,
		ingressClass:  ingressClass,
		containerPort: containerPort,
		servicePort:   servicePort,
	}
}

type execRunner struct{ kubeconfig, kubeContext string }

// Run executes kubectl with args, optionally piping stdin, and returns its
// combined output.
func (r *execRunner) Run(ctx context.Context, args []string, stdin string) (string, error) {
	if r.kubeconfig != "" {
		args = append(args, "--kubeconfig", r.kubeconfig)
	}
	if r.kubeContext != "" {
		args = append(args, "--context", r.kubeContext)
	}
	//nolint:gosec // args are built by this package from validated names, never from webhook input
	command := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("kubectl: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// deploymentName derives a stable, RFC 1123-valid object name from the project and
// branch. It hashes rather than slugifies because branch names routinely contain
// slashes and uppercase, and because two different branches must never collide on
// one name — a slug truncated to fit the length limit can.
func deploymentName(project, branch string) string {
	sum := sha256.Sum256([]byte(project + "/" + branch))
	return "ramify-" + hex.EncodeToString(sum[:])[:16]
}

// validName reports whether ref is safe to interpolate into a manifest and pass to
// kubectl as an object name. Apply reuses the deploy_ref the store handed back, so
// this is the boundary that keeps a corrupted or hand-edited row from reaching a
// manifest.
func validName(ref string) bool {
	return ref != "" && len(ref) <= maxNameLength && namePattern.MatchString(ref)
}

func hostName(subdomain, baseDomain string) string {
	if baseDomain == "" {
		return subdomain
	}
	return subdomain + "." + baseDomain
}

// quoteYAML renders value as a YAML double-quoted scalar. YAML's double-quoted
// escapes are a superset of Go's for the printable ASCII that image references and
// hostnames consist of, so strconv.Quote is exact for those; anything outside that
// set is rejected before it reaches a manifest.
func quoteYAML(value string) string { return strconv.Quote(value) }

// printableASCII reports whether value can be safely rendered by quoteYAML.
func printableASCII(value string) bool {
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func tlsSecretName(domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return "ramify-tls-" + hex.EncodeToString(sum[:])[:16]
}

func (p *Provider) manifest(name string, spec providerapi.EnvSpec) string {
	host := hostName(spec.Subdomain, p.baseDomain)
	class := ""
	if p.ingressClass != "" {
		class = "\n    ingressClassName: " + quoteYAML(p.ingressClass)
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  labels:
    app.kubernetes.io/managed-by: ramify
    ramify.io/deployment: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      ramify.io/deployment: %s
  template:
    metadata:
      labels:
        ramify.io/deployment: %s
    spec:
      containers:
      - name: app
        image: %s
        imagePullPolicy: IfNotPresent
        ports:
        - name: http
          containerPort: %d
---
apiVersion: v1
kind: Service
metadata:
  name: %s
  labels:
    app.kubernetes.io/managed-by: ramify
    ramify.io/deployment: %s
spec:
  selector:
    ramify.io/deployment: %s
  ports:
  - name: http
    port: %d
    targetPort: %d
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: %s
  labels:
    app.kubernetes.io/managed-by: ramify
    ramify.io/deployment: %s
spec:%s
  tls:
  - hosts:
    - %s
    secretName: %s
  rules:
  - host: %s
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: %s
            port:
              number: %d
`, name, name, name, name, quoteYAML(spec.ArtifactRef), p.containerPort,
		name, name, name, p.servicePort, p.containerPort,
		name, name, class, quoteYAML(host), tlsSecretName(host), quoteYAML(host), name, p.servicePort)
}

// Apply implements providerapi.DeployProvider. It is idempotent by construction:
// the object name is derived from Project and Branch, so a second call with the
// same EnvSpec re-applies the same three objects rather than creating new ones.
func (p *Provider) Apply(ctx context.Context, spec providerapi.EnvSpec) (providerapi.Deployment, error) {
	name := deploymentName(spec.Project, spec.Branch)
	if spec.PreviousRef != "" {
		if !validName(spec.PreviousRef) {
			return providerapi.Deployment{}, fmt.Errorf("kubernetes: apply: %w: previous deploy ref %q is not a valid object name",
				providerapi.ErrPermanent, spec.PreviousRef)
		}
		name = spec.PreviousRef
	}
	host := hostName(spec.Subdomain, p.baseDomain)
	if !printableASCII(spec.ArtifactRef) || !printableASCII(host) {
		return providerapi.Deployment{}, fmt.Errorf("kubernetes: apply %s: %w: artifact ref or host contains non-printable characters",
			name, providerapi.ErrPermanent)
	}
	if _, err := p.runner.Run(ctx, []string{"apply", "-n", p.namespace, "-f", "-"}, p.manifest(name, spec)); err != nil {
		return providerapi.Deployment{}, fmt.Errorf("kubernetes: apply %s: %w", name, err)
	}
	return providerapi.Deployment{Ref: name, InternalAddr: p.dnsTarget}, nil
}

// Sleep implements providerapi.DeployProvider by scaling the Deployment to zero
// replicas, leaving the Service and Ingress in place so Wake is cheap.
func (p *Provider) Sleep(ctx context.Context, ref string) error {
	return p.scale(ctx, ref, 0)
}

// Wake implements providerapi.DeployProvider by scaling a slept Deployment back to
// one replica.
func (p *Provider) Wake(ctx context.Context, ref string) error {
	return p.scale(ctx, ref, 1)
}

func (p *Provider) scale(ctx context.Context, ref string, replicas int) error {
	if !validName(ref) {
		return fmt.Errorf("kubernetes: scale: %w: invalid deploy ref %q", providerapi.ErrPermanent, ref)
	}
	args := []string{"scale", "deployment/" + ref, "-n", p.namespace, "--replicas=" + strconv.Itoa(replicas)}
	if _, err := p.runner.Run(ctx, args, ""); err != nil {
		return fmt.Errorf("kubernetes: scale %s to %d: %w", ref, replicas, err)
	}
	return nil
}

// Destroy implements providerapi.DeployProvider. --ignore-not-found makes a second
// Destroy on the same ref a no-op rather than an error, as the contract requires.
func (p *Provider) Destroy(ctx context.Context, ref string) error {
	if !validName(ref) {
		return fmt.Errorf("kubernetes: destroy: %w: invalid deploy ref %q", providerapi.ErrPermanent, ref)
	}
	args := []string{"delete", "deployment,service,ingress", ref, "-n", p.namespace, "--ignore-not-found"}
	if _, err := p.runner.Run(ctx, args, ""); err != nil {
		return fmt.Errorf("kubernetes: destroy %s: %w", ref, err)
	}
	return nil
}

// HealthCheck implements providerapi.DeployProvider, reporting healthy once the
// Deployment has at least one ready replica.
func (p *Provider) HealthCheck(ctx context.Context, ref string) (providerapi.Status, error) {
	if !validName(ref) {
		return providerapi.Status{}, fmt.Errorf("kubernetes: health check: %w: invalid deploy ref %q", providerapi.ErrPermanent, ref)
	}
	out, err := p.runner.Run(ctx, []string{"get", "deployment/" + ref, "-n", p.namespace, "-o", "jsonpath={.status.readyReplicas}"}, "")
	if err != nil {
		return providerapi.Status{}, fmt.Errorf("kubernetes: health check %s: %w", ref, err)
	}
	ready := strings.TrimSpace(out)
	// jsonpath renders an absent readyReplicas as the empty string, which is how
	// a Deployment reports "no pod has passed its readiness probe yet".
	if ready == "" || ready == "0" {
		return providerapi.Status{Healthy: false, Detail: "no ready replicas"}, nil
	}
	return providerapi.Status{Healthy: true, Detail: ready + " ready replica(s)"}, nil
}

// InstallCertificate stores the ACME certificate in the namespaced TLS Secret that
// the Ingress generated by manifest already references by name. Until this runs
// the Secret does not exist, and the ingress controller serves its default
// certificate for the host; applying it here is what completes TLS termination.
//
// It satisfies the optional certificate-installer capability the reconciler
// type-asserts for, and is deliberately not part of providerapi.DeployProvider.
func (p *Provider) InstallCertificate(ctx context.Context, domain string, certificatePEM, privateKeyPEM []byte) error {
	name := tlsSecretName(domain)
	// kubectl create secret tls cannot read two independent files from stdin, so
	// the Secret manifest is generated directly with both values base64-encoded.
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  labels:
    app.kubernetes.io/managed-by: ramify
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
`, name, base64.StdEncoding.EncodeToString(certificatePEM), base64.StdEncoding.EncodeToString(privateKeyPEM))
	if _, err := p.runner.Run(ctx, []string{"apply", "-n", p.namespace, "-f", "-"}, manifest); err != nil {
		return fmt.Errorf("kubernetes: install certificate %s: %w", domain, err)
	}
	return nil
}

// RemoveCertificate deletes the TLS Secret InstallCertificate created for domain.
// --ignore-not-found makes removing an already-absent Secret a no-op, matching the
// idempotent-teardown requirement on Destroy.
func (p *Provider) RemoveCertificate(ctx context.Context, domain string) error {
	name := tlsSecretName(domain)
	if _, err := p.runner.Run(ctx, []string{"delete", "secret", name, "-n", p.namespace, "--ignore-not-found"}, ""); err != nil {
		return fmt.Errorf("kubernetes: remove certificate %s: %w", domain, err)
	}
	return nil
}

// Logs satisfies the optional api.LogFetcher capability, returning the tail of the
// Deployment's pod logs for `ramify logs`.
func (p *Provider) Logs(ctx context.Context, ref string) (string, error) {
	if !validName(ref) {
		return "", fmt.Errorf("kubernetes: logs: %w: invalid deploy ref %q", providerapi.ErrPermanent, ref)
	}
	out, err := p.runner.Run(ctx, []string{"logs", "deployment/" + ref, "-n", p.namespace, "--all-containers", "--tail=500"}, "")
	if err != nil {
		return "", fmt.Errorf("kubernetes: logs %s: %w", ref, err)
	}
	return out, nil
}
