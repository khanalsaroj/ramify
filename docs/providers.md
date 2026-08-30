# Providers

Ramify's core (`internal/core`, `internal/store`, `internal/api`) never talks to
Git hosts, deploy targets, DNS APIs, ACME CAs, or notification channels directly —
it only knows the five interfaces in `providers/providerapi`:
`GitProvider`, `DeployProvider`, `DNSProvider`, `CertificateProvider`, and
`NotifierProvider`. Everything under `providers/` is a concrete implementation of
one or more of those.

| Interface | Built-in implementation |
|---|---|
| `GitProvider` | `providers/git/github`, `providers/git/gitlab`, `providers/git/bitbucket` |
| `DeployProvider` | `providers/deploy/compose` (SSH + `docker compose`), `providers/deploy/kubernetes` (`kubectl`) |
| `DNSProvider` | `providers/dns/cloudflare`, `providers/dns/route53`, `providers/dns/googlecloud`, `providers/dns/digitalocean` |
| `CertificateProvider` | `providers/cert/acme` (Let's Encrypt via DNS-01) |
| `NotifierProvider` | `providers/notify/prcomment` |

## The contract test suites

`test/contract` holds one exported `Run*Contract` function per interface —
`RunDeployProviderContract`, `RunDNSProviderContract`, `RunGitProviderContract`,
`RunCertificateProviderContract`, `RunNotifierProviderContract` — each expressing
the minimum behavior *any* implementation of that interface must satisfy (built-in
or a future third-party one): idempotent create/update, correct teardown, and —
for `DNSProvider` — that deleting a record you don't own is rejected, not silently
skipped. `RunNotifierProviderContract` is deliberately thin (a well-formed event
must deliver without error) since most of what a notifier does — which
`NotifyEvent.Kind`s have default templates, whether a zero `prNumber` is a no-op —
is implementation choice, not part of the interface's contract.

Every built-in provider's own test file (`providers/*/*/*.go`'s `_test.go`
counterpart) runs the relevant contract suite. In CI, none of them touch a real
external account or host — per the testing rules in the build spec (§7 item 1, no
real network/SSH/API calls in unit tests), each wires the *real* provider type
against a small in-memory test double standing in for the network:

- `providers/dns/cloudflare`: a `fakeCFClient` implementing the same narrow
  `dnsClient` interface the real `*cloudflare.API` satisfies.
- `providers/deploy/compose`: a `fakeComposeHost` simulating `docker compose`'s
  state transitions, standing in for the SSH `commandRunner`.
- `providers/cert/acme`: not run against a fake CA — `lego.NewClient` dials the
  ACME directory as part of construction, so there's no clean seam for a fake at
  the unit level. The DNS-01 challenge adapter and certificate-parsing helper are
  unit tested directly; `RunCertificateProviderContract` itself runs in
  `test/e2e/cert_contract_test.go` against the real ACME provider talking to
  Pebble, since that's the only account this contract can run against.
- `providers/notify/prcomment`: `RunNotifierProviderContract` runs in
  `providers/notify/prcomment/prcomment_test.go` against the real `Provider` wired
  to `test/fakes`' in-memory `GitProvider` — there's no real-account run for it,
  for the same reason `CommentOnPR` has none below.

## Running the contract suite against a real account

The fakes prove the provider's own logic (idempotency, error wrapping, ownership
checks) is correct independent of the network. They don't prove the real API calls
are correct. To check that, point the real provider at a real account:

### Git hosting providers

Select the provider in `ramify.yaml`:

```yaml
git:
  provider: gitlab # github, gitlab, or bitbucket
  token: $RAMIFY_GIT_TOKEN
  webhook_secret: $RAMIFY_GIT_WEBHOOK_SECRET
  base_url: https://gitlab.example.com # optional; useful for self-hosted GitLab
```

The webhook URL is `/webhooks/<provider>`, for example `/webhooks/gitlab`. That
path segment is cosmetic, so each host can be pointed at a URL that looks native
to it: a daemon has exactly one Git provider configured, and the header names
below come from that provider rather than from the URL.

| Provider | Signature header | Delivery ID header | Verification |
|---|---|---|---|
| GitHub | `X-Hub-Signature-256` | `X-GitHub-Delivery` | HMAC-SHA256 over the raw body |
| GitLab | `X-Gitlab-Token` | `X-Gitlab-Event-UUID` | Constant-time compare of the secret token |
| Bitbucket Cloud | `X-Hub-Signature` | `X-Hook-UUID` | HMAC-SHA256 over the raw body |

GitLab echoes the configured secret back rather than signing the payload, which is
why its verification is a token comparison and not an HMAC. In all three cases a
provider configured with an empty secret rejects every delivery rather than
accepting unsigned ones.

The normalized core behavior is identical across all three: branch pushes create
or update environments, pull/merge request updates refresh them, and closed
requests destroy them. Branch deletion also maps to `branch_deleted` on every
provider — GitHub sends `deleted: true`, GitLab sends an all-zero `after` commit,
and Bitbucket sends a push change whose `new` is `null`.

### DNS providers

DNS provider selection is configured independently from the Git provider:

```yaml
dns:
  provider: route53 # cloudflare, route53, googlecloud, or digitalocean
  zone: preview.example.com
  zone_id: Z123456789 # Google managed-zone name or optional Route 53 hosted-zone ID
  project: my-gcp-project # Google Cloud only
  api_token: $RAMIFY_DNS_TOKEN # DigitalOcean; Cloudflare may use cloudflare_api_token
```

Route 53 uses the AWS SDK default credential chain. Google Cloud DNS uses
Application Default Credentials. DigitalOcean uses a bearer API token. All four
providers implement the same TXT ownership-marker behavior, so Ramify refuses to
overwrite an unmanaged A/CNAME record and refuses deletes with the wrong tag.

### Cloudflare

```go
package main

import (
    "testing"

    "github.com/khanalsaroj/ramify/providers/dns/cloudflare"
    "github.com/khanalsaroj/ramify/test/contract"
)

func TestCloudflareContractLive(t *testing.T) {
    token := os.Getenv("RAMIFY_TEST_CLOUDFLARE_TOKEN")
    if token == "" {
        t.Skip("RAMIFY_TEST_CLOUDFLARE_TOKEN not set")
    }
    p, err := cloudflare.New(token)
    if err != nil {
        t.Fatal(err)
    }
    contract.RunDNSProviderContract(t, p, os.Getenv("RAMIFY_TEST_CLOUDFLARE_ZONE"))
}
```

Use a token scoped to a zone you don't mind the suite writing test `A`/`TXT`
records to and deleting again (it cleans up after itself, but scope the token
narrowly regardless — see the "no secret value in logs" rule in the build spec §6,
which this repo's own code follows for the same reason).

### Compose / SSH

```go
package main

import (
    "os"
    "testing"

    "golang.org/x/crypto/ssh"

    "github.com/khanalsaroj/ramify/providers/deploy/compose"
    "github.com/khanalsaroj/ramify/test/contract"
)

func TestComposeContractLive(t *testing.T) {
    keyPath := os.Getenv("RAMIFY_TEST_SSH_KEY")
    addr := os.Getenv("RAMIFY_TEST_SSH_ADDR")
    if keyPath == "" || addr == "" {
        t.Skip("RAMIFY_TEST_SSH_KEY / RAMIFY_TEST_SSH_ADDR not set")
    }
    keyBytes, err := os.ReadFile(keyPath)
    if err != nil {
        t.Fatal(err)
    }
    signer, err := ssh.ParsePrivateKey(keyBytes)
    if err != nil {
        t.Fatal(err)
    }
    p := compose.New(addr, "ramify", signer, ssh.InsecureIgnoreHostKey(), "/srv/ramify/docker-compose.yml", addr)
    contract.RunDeployProviderContract(t, p)
}
```

Point this at a disposable host (or the same `test/e2e` fake-`docker`-shim sshd
image, run standalone) — the contract suite really does run `docker compose up`
and `down` against whatever `compose_file` you configure.

### Kubernetes

Set `deploy.provider: kubernetes`. Ramify invokes the local `kubectl` binary using
the configured kubeconfig/context and creates a Deployment, Service, and Ingress
for each preview. The configured `deploy.dns_target` should be the address of the
cluster ingress/load-balancer entry point. Container and Service ports default to
`8080` and can be changed with `kubernetes_container_port` and
`kubernetes_service_port`.

```yaml
deploy:
  provider: kubernetes
  dns_target: 203.0.113.20
  kubernetes_namespace: ramify
  kubernetes_context: production
  kubernetes_ingress_class: nginx
  kubernetes_container_port: 8080
  kubernetes_service_port: 80
```

The Kubernetes provider uses the same idempotent Apply/Sleep/Wake/Destroy
contract as Compose. It expects `kubectl` and cluster credentials to be available
on the machine running `ramifyd`. Sleep scales the Deployment to zero replicas and
leaves the Service and Ingress in place, so Wake is a scale back to one rather
than a redeploy.

Object names are derived by hashing `project/branch`, not by slugifying it:
branch names routinely contain slashes and uppercase, and a slug truncated to fit
Kubernetes' 63-character limit can collide across two different branches. A
`deploy_ref` read back from the store is validated against the same RFC 1123 rules
before it reaches a manifest.

The generated Ingress references a TLS secret named `ramify-tls-<hash of host>`.
That secret does not exist until the ACME certificate is issued, at which point
the reconciler calls the provider's `InstallCertificate` and creates it — so a
freshly applied preview serves the ingress controller's default certificate for a
short window before switching to its own.

### GitHub

`RunGitProviderContract` needs a set of pre-signed webhook fixtures, not a live
account (there's no "run a webhook against yourself" flow) — see
`providers/git/github/github_test.go` for the fixtures it already exercises.
`CommentOnPR` against a real repository is covered by `test/e2e`'s mock GitHub
server standing in for the real API; there isn't a separate "real account" contract
run for it, since posting a comment to a real PR from a test suite would be a
visible side effect on someone's real repository.

GitLab and Bitbucket use the same provider contract. Their API calls should be
verified against a disposable project because comment posting is an external side
effect.

## Operational dashboard

When `server.tcp_addr` is enabled, open `/dashboard/` on that listener. The
dashboard lists environments, filters by project/branch/status, opens preview
URLs, performs sleep/wake/destroy operations, and tails deployment logs. It is
embedded in the binary as a single file with no external assets — no CDN, no
fonts, no build step — so it works unchanged on an air-gapped network.

The page asks for the same `server.tcp_token` the CLI uses and keeps it in browser
storage only. Every route that reads or changes an environment stays
bearer-authenticated; `/dashboard/` and `/dashboard/config` are deliberately
exempt, because a browser cannot attach an `Authorization` header to a top-level
navigation. What that exemption serves is a static page and the base domain — no
environment data — but it does mean the listener itself should sit on a trusted
network. Only `GET` and `HEAD` are routed to the asset handler.

Destroy asks for the branch name to be typed back before it will run, and issues
`DELETE /environments/{id}` — the same call `ramify destroy` makes.

## The e2e harness

`test/e2e` brings up all of the above for real (Pebble for ACME, CoreDNS for DNS,
a fake SSH deploy target, a mock GitHub API) and drives the full
create → verify → destroy loop. It runs as a gating CI job on every push and pull
request. To run it yourself:

```sh
docker compose -f test/e2e/docker-compose.dev.yml run --build --rm test-runner
docker compose -f test/e2e/docker-compose.dev.yml down --volumes
```

`run` rather than `up`: it waits on the `depends_on` conditions and exits with the
test's own status, which `up --abort-on-container-exit` cannot do here — the
one-shot zone-seeding container would abort the stack the moment it finished.

Two deliberate substitutions. DNS goes through an e2e-only file-based provider
(`test/e2e/dnsfile`) rather than the real Cloudflare provider, so the harness needs
no account and no network egress — CoreDNS serves the zone file it writes, which is
enough for Pebble to validate a real DNS-01 challenge. And the reconciler and real
provider implementations are wired directly in a Go test process rather than
running the compiled `ramifyd` binary, so a failure surfaces as a Go stack trace at
the assertion rather than as a line in a container log.
