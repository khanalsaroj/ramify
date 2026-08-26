// Package kubernetes implements DeployProvider using kubectl and a Kubernetes
// Deployment, Service, and Ingress per preview environment.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/khanalsaroj/ramify/providers/providerapi"
)

const maxNameLength = 63

type commandRunner interface {
	Run(ctx context.Context, args []string, stdin string) (string, error)
}

// Provider manages one Kubernetes namespace and uses DNSTarget as the address
// of the cluster ingress/load balancer. Ingress rules route each preview host to
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

// New constructs a provider backed by kubectl. Authentication and cluster
// selection are delegated to kubeconfig, the active context, and standard
// kubectl environment handling.
func New(namespace, baseDomain, dnsTarget, ingressClass, kubeconfig, kubeContext string, containerPort, servicePort int) *Provider {
	if namespace == "" {
		namespace = "default"
	}
	if containerPort == 0 {
		containerPort = 8080
	}
	if servicePort == 0 {
		servicePort = containerPort
	}
	return NewWithRunner(&execRunner{kubeconfig: kubeconfig, kubeContext: kubeContext}, namespace, baseDomain, dnsTarget, ingressClass, containerPort, servicePort)
}

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
	return &Provider{runner: r, namespace: namespace, baseDomain: strings.TrimSuffix(baseDomain, "."), dnsTarget: dnsTarget, ingressClass: ingressClass, containerPort: containerPort, servicePort: servicePort}
}

type execRunner struct{ kubeconfig, kubeContext string }

func (r *execRunner) Run(ctx context.Context, args []string, stdin string) (string, error) {
	command := exec.CommandContext(ctx, "kubectl", args...)
	if r.kubeconfig != "" {
		command.Args = append(command.Args, "--kubeconfig", r.kubeconfig)
	}
	if r.kubeContext != "" {
		command.Args = append(command.Args, "--context", r.kubeContext)
	}
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("kubectl: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func projectName(project, branch string) string {
	sum := sha256.Sum256([]byte(project + "/" + branch))
	return "ramify-" + hex.EncodeToString(sum[:])[:16]
}

func hostName(subdomain, baseDomain string) string {
	if baseDomain == "" {
		return subdomain
	}
	return subdomain + "." + baseDomain
}

func quoteYAML(value string) string { return strconv.Quote(value) }

func tlsSecretName(domain string) string {
	return "ramify-tls-" + hex.EncodeToString(sha256Bytes(domain))[:16]
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
		name, name, class, quoteYAML(host), quoteYAML(host), tlsSecretName(host), name, p.servicePort)
}

func (p *Provider) Apply(ctx context.Context, spec providerapi.EnvSpec) (providerapi.Deployment, error) {
	name := projectName(spec.Project, spec.Branch)
	if spec.PreviousRef != "" {
		name = spec.PreviousRef
	}
	if _, err := p.runner.Run(ctx, []string{"apply", "-n", p.namespace, "-f", "-"}, p.manifest(name, spec)); err != nil {
		return providerapi.Deployment{}, fmt.Errorf("kubernetes: apply %s: %w", name, err)
	}
	return providerapi.Deployment{Ref: name, InternalAddr: p.dnsTarget}, nil
}

func (p *Provider) Sleep(ctx context.Context, ref string) error {
	if _, err := p.runner.Run(ctx, []string{"scale", "deployment/" + ref, "-n", p.namespace, "--replicas=0"}, ""); err != nil {
		return fmt.Errorf("kubernetes: sleep %s: %w", ref, err)
	}
	return nil
}

func (p *Provider) Wake(ctx context.Context, ref string) error {
	if _, err := p.runner.Run(ctx, []string{"scale", "deployment/" + ref, "-n", p.namespace, "--replicas=1"}, ""); err != nil {
		return fmt.Errorf("kubernetes: wake %s: %w", ref, err)
	}
	return nil
}

func (p *Provider) Destroy(ctx context.Context, ref string) error {
	args := []string{"delete", "deployment,service,ingress", ref, "-n", p.namespace, "--ignore-not-found"}
	if _, err := p.runner.Run(ctx, args, ""); err != nil {
		return fmt.Errorf("kubernetes: destroy %s: %w", ref, err)
	}
	return nil
}

func (p *Provider) HealthCheck(ctx context.Context, ref string) (providerapi.Status, error) {
	out, err := p.runner.Run(ctx, []string{"get", "deployment/" + ref, "-n", p.namespace, "-o", "jsonpath={.status.readyReplicas}"}, "")
	if err != nil {
		return providerapi.Status{}, fmt.Errorf("kubernetes: health check %s: %w", ref, err)
	}
	if strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "0" {
		return providerapi.Status{Healthy: false, Detail: "no ready replicas"}, nil
	}
	return providerapi.Status{Healthy: true, Detail: strings.TrimSpace(out) + " ready replica(s)"}, nil
}

// InstallCertificate stores the ACME certificate in a namespaced TLS Secret.
// The current Ingress intentionally does not set tls until certificate material
// is available; the secret is still created for operators or ingress templates
// that reference it.
func (p *Provider) InstallCertificate(ctx context.Context, domain string, certificatePEM, privateKeyPEM []byte) error {
	name := tlsSecretName(domain)
	// kubectl cannot consume two independent stdin files. Generate the Secret
	// manifest directly so both values are passed as base64 data.
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
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

func sha256Bytes(value string) []byte { sum := sha256.Sum256([]byte(value)); return sum[:] }

// Logs is an optional capability used by the CLI.
func (p *Provider) Logs(ctx context.Context, ref string) (string, error) {
	out, err := p.runner.Run(ctx, []string{"logs", "deployment/" + ref, "-n", p.namespace, "--all-containers", "--tail=500"}, "")
	if err != nil {
		return "", fmt.Errorf("kubernetes: logs %s: %w", ref, err)
	}
	return out, nil
}
