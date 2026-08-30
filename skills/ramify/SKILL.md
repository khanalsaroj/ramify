---
name: ramify
description: How to set up, configure, operate, and troubleshoot Ramify — the self-hosted preview-environment control plane in this repo (the `ramify` CLI and `ramifyd` daemon). Use when working with ramify.yaml, the ramify CLI, a GitHub/GitLab/Bitbucket webhook, the Compose or Kubernetes deploy target, the Cloudflare/Route 53/Google Cloud/DigitalOcean DNS providers, the operational dashboard, branch/label filtering and environment caps, DNS/TLS lifecycle, TTL expiry, or when diagnosing why a preview environment did not come up.
---

# Using Ramify

Ramify turns Git branches and pull requests into short-lived preview
environments with real DNS and real certificates, on infrastructure the
operator already runs. Push a branch → `feature-x.preview.example.com` is live.
Close the PR or let the TTL lapse → deploy, DNS record, and certificate are torn
down together.

## Two binaries

| Binary    | Runs where                         | Job                                                                              |
|-----------|------------------------------------|----------------------------------------------------------------------------------|
| `ramifyd` | On the VPS/control host            | The daemon: config, providers, reconciler, reaper, control API, webhook receiver |
| `ramify`  | Anywhere with access to the daemon | The CLI: talks to `ramifyd` over its local control API                           |

`ramify` reaches `ramifyd` over a unix socket at `/var/run/ramify/ramify.sock`
by default. Override globally with `--socket <path>`, or use TCP with
`--addr host:port --token <bearer>` (`--addr` wins over `--socket`).

## Which providers are configured

Four provider slots, each chosen by a `provider:` key in `ramify.yaml`. Every
combination is valid; the daemon constructs exactly one of each at startup and
fails fast on an unknown name.

| Slot   | Key               | Values                                                 | Default      |
|--------|-------------------|--------------------------------------------------------|--------------|
| Git    | `git.provider`    | `github`, `gitlab`, `bitbucket`                        | `github`     |
| Deploy | `deploy.provider` | `compose`, `kubernetes`                                | `compose`    |
| DNS    | `dns.provider`    | `cloudflare`, `route53`, `googlecloud`, `digitalocean` | `cloudflare` |
| Cert   | (none)            | ACME/Let's Encrypt via DNS-01, the only implementation | (n/a)        |

Per-provider setup, credentials, and the webhook headers each Git host sends:
`docs/providers.md`. Health/readiness endpoints, backups, durable event
processing, and dead-lettering: `docs/operations.md`.

## Read this first — two things Ramify does NOT do

These cause most of the confusion, and neither is a bug:

1. **Ramify never builds images.** The reconciler's contract is explicit:
   `DeployProvider.Apply` only ever receives an already-built artifact ref. That
   ref is the **head commit SHA** from the webhook. Your CI must build and push
   an image for that SHA *before* Ramify deploys it.
2. **With Compose, Ramify does not route traffic per hostname.** It points DNS
   at a single address (`deploy.dns_target`) and brings the container up.
   Getting `feature-x.preview.example.com` to the right container is your
   reverse proxy's job — Traefik or Caddy with Docker labels on the Compose
   service. The Kubernetes provider is the exception: it writes an Ingress that
   routes on the host, so an ingress controller handles this for you.

## The Compose contract

This section applies when `deploy.provider` is `compose`. For `kubernetes`, skip
to "The Kubernetes contract" below.

Ramify runs this over SSH on the deploy host:

```sh
IMAGE_TAG=<commit-sha> COMPOSE_PROJECT_NAME=ramify-<project>-<branch> \
  docker compose -f <deploy.compose_file> up -d
```

So the Compose file on the VPS must consume `IMAGE_TAG`. Because `IMAGE_TAG` is
a bare commit SHA, interpolate it into a full image reference — don't use it as
the entire `image:` value unless your registry setup makes a bare SHA
resolvable:

```yaml
services:
  app:
    image: ghcr.io/OWNER/REPO:${IMAGE_TAG}
    restart: unless-stopped
    labels:
    # your reverse proxy's routing rules go here
```

One Compose file serves every environment; isolation comes from
`COMPOSE_PROJECT_NAME`. That's why running `up -d` twice for the same branch is
an update rather than a second deployment.

## The Kubernetes contract

With `deploy.provider: kubernetes`, Ramify shells out to `kubectl` — not
client-go — so authentication, contexts, and proxy handling stay whatever the
operator already configured. `kubectl` must be on `ramifyd`'s PATH, and the
namespace in `deploy.kubernetes_namespace` must already exist; Ramify does not
create it.

Each environment becomes a Deployment, a Service, and an Ingress applied
together, all sharing one object name. That name is a **hash** of project and
branch, not a slug: branch names routinely contain slashes and uppercase, and a
slug truncated to fit RFC 1123's 63-character limit can collide across two
different branches, which would let one preview overwrite another.

The Ingress routes on the environment's host and terminates TLS with a secret
named `ramify-tls-<hash of the host>`. Ramify creates that secret in
`InstallCertificate`, which runs *after* the manifest is applied — so for the
few seconds between deploy and certificate issuance the Ingress references a
secret that does not exist yet, and the controller serves its own default
certificate. That is expected on a fresh preview, not a failure.

`Sleep` scales the Deployment to zero replicas and `Wake` scales it back to one,
so a slept environment keeps its DNS record and its certificate. Compose has no
equivalent and returns an error for both.

**Destroy also removes the installed TLS material**, not just the DNS record
and ACME certificate: Kubernetes deletes the `ramify-tls-<hash>` Secret, and
Compose removes the certificate/key files it wrote under
`deploy.certificate_dir` over SSH. Both operations are idempotent
(`--ignore-not-found`, `rm -f`), consistent with the rest of teardown.

## Setup, start to finish

```sh
# 1. Install both binaries (Linux/macOS)
curl -fsSL https://raw.githubusercontent.com/khanalsaroj/ramify/main/scripts/install.sh | bash
#    Windows: iwr -useb .../scripts/install.ps1 | iex   (restart the terminal after)
#    Pin with RAMIFY_VERSION, relocate with RAMIFY_INSTALL_DIR.

# 2. Create the directories  (NOTE: does not install binaries — see gotchas)
ramify install --config-dir /etc/ramify --data-dir /var/lib/ramify

# 3. Deploy key + host key
ssh-keygen -t ed25519 -N "" -f /etc/ramify/deploy_key
ssh-copy-id -i /etc/ramify/deploy_key.pub ramify@YOUR_VPS_IP
ssh-keyscan -t ed25519 YOUR_VPS_IP >> /etc/ramify/known_hosts

# 4. Put the Compose file on the VPS (see "The Compose contract")

# 5. Generate config — secrets stay as $NAME references, never literals
#    Defaults below are github + compose + cloudflare. Add --git-provider,
#    --deploy-provider, or --dns-provider to select others.
export RAMIFY_GITHUB_TOKEN=ghp_...
export RAMIFY_GITHUB_WEBHOOK_SECRET=$(openssl rand -hex 32)
export RAMIFY_CLOUDFLARE_API_TOKEN=...
ramify init --output /etc/ramify/ramify.yaml \
  --base-domain preview.example.com \
  --github-token '$RAMIFY_GITHUB_TOKEN' \
  --github-webhook-secret '$RAMIFY_GITHUB_WEBHOOK_SECRET' \
  --deploy-ssh-addr YOUR_VPS_IP:22 --deploy-ssh-key /etc/ramify/deploy_key \
  --deploy-ssh-known-hosts /etc/ramify/known_hosts \
  --deploy-compose-file /srv/ramify/docker-compose.yml \
  --deploy-dns-target YOUR_VPS_IP \
  --dns-zone preview.example.com --cloudflare-token '$RAMIFY_CLOUDFLARE_API_TOKEN' \
  --acme-email you@example.com

# 6. Validate before starting
ramify doctor --config /etc/ramify/ramify.yaml

# 7. Run
sudo ramifyd --config /etc/ramify/ramify.yaml
```

Quote the `$NAME` flag values in single quotes so the shell passes the reference
through literally — `ramify init` writes it to the file as given, and `ramifyd`
resolves it from the environment at load time.

Then add the webhook on the repository. The route accepts any provider segment
(`/webhooks/github`, `/webhooks/gitlab`, `/webhooks/bitbucket`); the segment is
cosmetic, since a daemon has exactly one Git provider configured and asks that
provider which headers to read.

| Host      | Webhook URL                            | Secret field | Events to subscribe to                                        |
|-----------|----------------------------------------|--------------|---------------------------------------------------------------|
| GitHub    | `https://your-host/webhooks/github`    | Secret       | Pull requests, Pushes                                          |
| GitLab    | `https://your-host/webhooks/gitlab`    | Secret token | Merge request events, Push events                              |
| Bitbucket | `https://your-host/webhooks/bitbucket` | Secret       | Pull request created/updated/merged/declined, Repository push  |

GitHub and Bitbucket sign the body with HMAC-SHA256; GitLab instead sends the
secret back verbatim in `X-Gitlab-Token`, which Ramify compares in constant
time. Either way an empty `git.webhook_secret` rejects every delivery rather
than accepting it.

Open a PR and watch it land:

```sh
ramify status your-branch-name    # ready == live
```

Full walkthrough including the systemd unit: `docs/quickstart.md`.

## Configuration

One YAML file. Any secret-bearing field accepts a literal **or** a `$NAME` /
`${NAME}` reference resolved from the environment at load time — resolution
fails loudly if the variable is unset. Ramify logs which secret fields were
configured, never their values.

```yaml
base_domain: preview.example.com     # feature-x -> feature-x.preview.example.com

server:
  socket_path: /var/run/ramify/ramify.sock
  # tcp_addr: 0.0.0.0:8443           # optional remote control API + dashboard
  # tcp_token: $RAMIFY_TCP_TOKEN
  # tcp_tls_cert_file: /etc/ramify/tls/cert.pem   # required unless tcp_addr is loopback
  # tcp_tls_key_file: /etc/ramify/tls/key.pem     # or tcp_insecure_allow_remote: true
store:
  path: /var/lib/ramify/ramify.db

reaper:
  interval: 5m                       # how often expiry is enforced
  default_ttl: 72h                   # refreshed on every successful apply
  event_retention: 720h              # how long completed events are kept
  event_concurrency: 8                # max events replayed in parallel (0 uses the default)

git:
  provider: github                   # github | gitlab | bitbucket
  token: $RAMIFY_GITHUB_TOKEN        # only needed to post PR comments
  webhook_secret: $RAMIFY_GITHUB_WEBHOOK_SECRET
  base_url: ""                       # set for self-hosted GitLab

deploy:
  provider: compose                  # compose | kubernetes
  ssh_addr: deploy-host.example.com:22
  ssh_user: ramify                   # default
  ssh_private_key_path: /etc/ramify/deploy_key
  ssh_known_hosts_path: /etc/ramify/known_hosts
  # ssh_insecure_skip_host_key_verify: true   # only if you truly can't pin a host key
  compose_file: /srv/ramify/docker-compose.yml
  dns_target: 203.0.113.10
  readiness_timeout: 0               # 0 uses the built-in default (2m)
  readiness_poll_interval: 0         # 0 uses the built-in default (2s)

dns:
  provider: cloudflare               # cloudflare | route53 | googlecloud | digitalocean
  zone: preview.example.com
  cloudflare_api_token: $RAMIFY_CLOUDFLARE_API_TOKEN

acme:
  email: ops@example.com
  ca_dir_url: https://acme-v02.api.letsencrypt.org/directory
  storage_dir: /var/lib/ramify/certificates

notify:
  comment_templates: {}             # override the text/template used per notify
                                     # kind: ready|updated|failed|expiring|destroyed

log:
  format: ""                        # json (default) | text; same as RAMIFY_LOG_FORMAT
```

A `github:` block carrying `token` and `webhook_secret` is still accepted and
copied into `git:` at load time, so an installation predating multi-provider
support keeps working untouched. New configs should use `git:`.

**Required at startup**, whatever the providers — `ramifyd` refuses to boot
without all of these: `base_domain`, `server.socket_path`, `store.path`,
`git.token`, `git.webhook_secret`, `deploy.dns_target`, `dns.zone`,
`acme.email`, `acme.ca_dir_url`.

**Also required, depending on the provider:**

| Condition                                    | Additionally required                                                   |
|----------------------------------------------|-------------------------------------------------------------------------|
| `deploy.provider: compose`                   | `deploy.ssh_addr`, `deploy.compose_file`, `deploy.ssh_private_key_path`, `deploy.certificate_dir`, `deploy.ssh_known_hosts_path` (or explicit `deploy.ssh_insecure_skip_host_key_verify: true`) |
| `deploy.provider: kubernetes`                | `deploy.kubernetes_namespace`                                           |
| `dns.provider: cloudflare` or `digitalocean` | `dns.api_token` (Cloudflare also accepts `dns.cloudflare_api_token`)     |
| `dns.provider: googlecloud`                  | `dns.project`, `dns.zone_id`                                            |
| `server.tcp_addr` set                        | `server.tcp_token`, and either `server.tcp_tls_cert_file`+`tcp_tls_key_file`, a loopback `tcp_addr`, or explicit `server.tcp_insecure_allow_remote: true` |

`deploy.certificate_dir` is required for Compose rather than optional: SSH is
the only route TLS material has to that host, so without it every apply obtains
a certificate and then fails installing it, five retries deep. Kubernetes does
not use it — it installs certificates as Secrets.

`deploy.ssh_known_hosts_path` is required for Compose for the same fail-closed
reason: without a pinned host key, SSH host-key verification has nothing safe
to fall back to, so `ramifyd` refuses to start rather than silently accepting
any host key. Set `ssh_insecure_skip_host_key_verify: true` only if you have
another way to guarantee you're talking to the right host (e.g. a private
network with no possibility of MITM).

Similarly, a non-loopback `server.tcp_addr` refuses to start without TLS
(`tcp_tls_cert_file` + `tcp_tls_key_file`) or an explicit
`tcp_insecure_allow_remote: true` override — plaintext bearer-token auth over
a network is a credential leak waiting to happen. A loopback `tcp_addr`
(`127.0.0.1:...`, `::1:...`, `localhost:...`) is exempt, since it never leaves
the host.

Route 53 and Google Cloud DNS need no token in the file: they use the AWS SDK
credential chain and Application Default Credentials respectively. For Route 53
`dns.zone_id` is optional — left empty, the hosted zone is resolved by name.

Annotated reference: `ramify.example.yaml`.

## CLI reference

| Command                   | Key flags                                                                                                                                         | Does                                                                         |
|---------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------|
| `ramify install`          | `--config-dir`, `--data-dir`                                                                                                                      | Creates config/data dirs. Does **not** install binaries.                     |
| `ramify init`             | `--output`, `--base-domain`, `--socket-path`, `--store-path`, `--default-ttl`, `--event-retention`, `--git-provider`, `--github-*`, `--deploy-provider`, `--deploy-*`, `--kubernetes-*`, `--dns-provider`, `--dns-zone`, `--cloudflare-token`, `--dns-token`, `--acme-email` | Writes `ramify.yaml` (mode 0600) non-interactively; validates before writing |
| `ramify list`             | `--project owner/repo`                                                                                                                            | Table: ID, PROJECT, BRANCH, STATUS, SUBDOMAIN, ARTIFACT                      |
| `ramify status <branch>`  | `--project`                                                                                                                                       | Full detail for one environment                                              |
| `ramify logs <branch>`    | `--project`                                                                                                                                       | Deploy logs (last 500 lines; Compose and Kubernetes both implement this)     |
| `ramify destroy <branch>` | `--project`, `-y`/`--yes`                                                                                                                         | Manual teardown; confirms unless `-y`                                        |
| `ramify doctor`           | `--config`                                                                                                                                        | Independently checks config, the configured DNS + deploy providers, webhook secret, ACME |
| `ramify backup`           | `--config`, `--output`                                                                                                                            | Consistent online copy of the SQLite state (safe while `ramifyd` is running) |

Global flags: `--socket`, `--addr`, `--token`. Per-command detail:
`ramify <cmd> --help`.

The webhook-secret and Git flags on `init` are still spelled `--github-token` /
`--github-webhook-secret` for every provider; they write `git.token` and
`git.webhook_secret`. Pair them with `--git-provider gitlab` (and
`--git-base-url` for self-hosted GitLab) as needed.

There is no `ramify sleep` / `ramify wake`. Sleep and wake exist on the control
API (`POST /environments/{id}/sleep`, `/wake`) and on the dashboard; only the
Kubernetes deploy provider implements them.

Branch-taking commands resolve `<branch>` to exactly one environment. If the
same branch name exists in several projects the command fails with
`multiple environments match branch "x"; pass --project to disambiguate`.

`GET /environments/` is paginated (`DefaultListLimit` 100, `MaxListLimit` 500,
`X-Ramify-Next-Offset` response header); `ramify list` and `ramify status`
follow it automatically, so a large fleet never needs manual pagination.

## Health, readiness, and metrics

`ramifyd` serves `/healthz` (state store is readable), `/readyz` (event store is
available for reconciliation), and `/metrics` (Prometheus text format — webhook
deliveries, duplicates, retries, reconciliation failures, cleanup failures,
pending inbox work, dead-lettered events). Put these behind the same
socket/authenticated-TCP boundary as the control API. Details: `docs/operations.md`.

## Lifecycle

Statuses: `pending` → `deploying` → `ready`, or `failed`. Teardown runs
`destroying` → `destroyed`. A `ready` environment can also go `sleeping` and
back. The store enforces this as a transition graph and rejects any jump that
isn't an edge in it, so a test or a caller cannot move an environment straight
from `pending` to `ready`.

**An environment isn't marked `ready` on a successful `Apply` alone.** After
the deploy provider reports success, the reconciler polls `HealthCheck` (every
`deploy.readiness_poll_interval`, default 2s) until it reports healthy or
`deploy.readiness_timeout` (default 2m) elapses — only then does DNS get
created and the certificate installed. A readiness timeout is just another
apply failure: it counts against the 5-attempt retry budget below rather than
leaving a half-healthy environment announced as ready.

Sleep and wake are operator-driven only: `/sleep` and `/wake` are exposed on the
control API and the dashboard, but **automatic idle-detection is not
implemented** (see "Scope" below). Only the Kubernetes deploy provider can act
on them.

Webhook events map to actions:

No provider reads the event-name header. `ParseWebhook` takes only the body and
the signature, so all three infer the event from the payload's shape and the
PR/MR state inside it:

| GitHub                                          | GitLab                                     | Bitbucket                                    | Ramify action            |
|-------------------------------------------------|--------------------------------------------|----------------------------------------------|--------------------------|
| `pull_request` opened / reopened / synchronize  | action `open` / `reopen` / `update`        | `pullrequest.state` is `OPEN`                | Apply (create or update) |
| `push` with a head commit                        | push with a non-zero `after`               | a change with a new branch target            | Apply                    |
| `pull_request` closed                            | state `merged` / `closed`, or action `close` / `merge` | state `MERGED` / `DECLINED` / `SUPERSEDED` | Destroy                  |
| branch deleted                                   | push whose `after` is all zeroes           | a change with `old` but no `new`             | Destroy                  |

Order matters in two places. A merged GitLab MR arrives as action `update` with
state `merged`, so **state is checked before action** — reading the action alone
would redeploy a merged MR instead of tearing it down. And a GitLab push with an
*empty* `after` is a malformed payload, not a deletion; only the all-zeroes SHA
means the branch is gone.

A Bitbucket push may batch several branches into one delivery, so non-branch
refs (tags) are skipped rather than rejected.

Anything else is acknowledged with 200 and ignored. Apply retries the
deploy/DNS/cert sequence up to **5 times** in-process before marking the
environment `failed`.

**Crash safety and durable events:** a webhook delivery is written to the store
and deduplicated by the Git host's delivery ID (`X-GitHub-Delivery`,
`X-Gitlab-Event-UUID`, `X-Hook-UUID`) *before* any provider is called; a
delivery without one is rejected outright since it can't be deduplicated on
redelivery. The webhook returns 202 immediately while work proceeds
asynchronously, and on restart `ramifyd` replays unprocessed events, so a
mid-flight crash resumes rather than losing work. Every provider operation is
idempotent.

On top of the 5-attempt apply retry above, the event itself gets a second,
durable retry budget — up to **10 attempts** with jittered backoff — before it's
marked dead-lettered rather than retried further. A pending event means the
desired state hasn't been confirmed yet; check `/metrics` and `ramifyd`'s logs
before touching provider resources by hand. Details: `docs/operations.md`.

**TTL:** each successful apply sets `ttl_expires_at = now + default_ttl`, so an
actively-pushed branch keeps renewing and only expires `default_ttl` after the
last push. The reaper sweeps on `reaper.interval`. Environments flagged `pinned`
are never swept regardless of TTL. `default_ttl: 0` disables expiry entirely.

## Filtering which branches get an environment

By default every branch push creates an environment. On a busy repository that
means every feature branch holds a DNS record, a certificate, and a running
container until its TTL lapses. The optional `filter:` block narrows that.

```yaml
filter:
  pr_only: false            # ignore pushes with no open pull/merge request
  allow_branches: []        # globs; empty allows everything deny does not reject
  deny_branches: []         # globs; evaluated first, and wins over allow
  required_labels: []       # pull request must carry one of these
  max_concurrent_envs: 0    # ceiling on live environments; 0 is unlimited
```

Omitting the block changes nothing, and so does an empty one: every rule is
opt-in, and the zero value is the behavior Ramify had before the block existed.

**Branch patterns follow the GitHub Actions convention**, which is what you
already have in your fingers from workflow files: `*` does not cross a slash,
`**` does. The trap worth internalizing is that `dependabot/*` does **not** match
`dependabot/npm/lodash`, because Dependabot nests two levels deep. Write
`dependabot/**`. A malformed pattern matches nothing rather than erroring, so a
broken deny rule cannot silently let branches through.

**`required_labels` is skipped, not enforced, where the host cannot report
labels.** Bitbucket Cloud has no pull request labels at all, and a bare branch
push has no request to carry them. Treating either as "carries no labels" would
silently disable previews entirely on Bitbucket and for every branch push, so
Ramify skips the rule instead. Set `pr_only: true` alongside it to close that
gap. A GitHub or GitLab request that genuinely has zero labels *is* gated —
that case is known-and-empty, not unknown.

**`max_concurrent_envs` rejects; it never evicts.** At the ceiling, a push for a
*new* environment is skipped and logged, and nothing already live is touched.
Pushes to environments that already exist still deploy, so reaching the ceiling
never freezes a live preview on a stale commit. Destroying an environment or
letting one expire frees the slot. The count covers every status except
`destroyed`, including `failed` and `pending`, since both can hold a partially
created record or container.

**Skips are logged, not commented on the pull request.** A denied branch pattern
matches on every push to that branch, and commenting each time would bury the
request in noise for what is a standing operator decision rather than an error.
The reason appears in `ramifyd`'s logs:

```
level=INFO msg="reconciler: event skipped by admission policy"
  project=acme/web branch=dependabot/npm/lodash
  reason="skipped: branch \"dependabot/npm/lodash\" matches deny pattern \"dependabot/**\""
```

`ramify init` writes the block from `--pr-only`, `--allow-branches`,
`--deny-branches`, `--required-labels`, and `--max-concurrent-envs`.

## Naming rules

Branch names become DNS labels via `internal/core/domain.Normalize`: lowercase →
`/` and `_` become `-` → strip anything outside `[a-z0-9-]` → collapse and trim
`-`. If the result exceeds the max length it's truncated and suffixed with `-`
plus the first 6 hex chars of `sha256(branch)`; if it normalizes to empty, the
hash alone is used. So `feature/ADD_Login` → `feature-add-login`. The same
normalization derives the Compose project name (`ramify-<project>-<branch>`),
which is why two branches never collide.

DNS records Ramify creates are tagged with a companion TXT ownership record
(external-dns style) — it will never modify or delete a record it doesn't own.

## Troubleshooting

1. **`ramify doctor` first.** It isolates each dependency and names the failure,
   dispatching on the providers you actually configured: the DNS provider's
   credentials (Cloudflare resolves the zone; DigitalOcean checks the token;
   Route 53 and Google Cloud only confirm the config, since their credentials
   come from ambient chains), the deploy target (SSH reachability + auth for
   Compose, `kubectl get namespace` for Kubernetes), webhook secret length
   (must be ≥ 16 chars), and ACME directory reachability. Note it dials SSH
   with host-key verification disabled for that one connectivity check — this
   is separate from `ramifyd` itself, which now refuses to start for Compose
   unless `ssh_known_hosts_path` is set or the insecure override is explicit.
2. **`ramify logs <branch>`** for the container's own output.
3. **`ramifyd` logs are structured JSON.** Set `RAMIFY_LOG_FORMAT=text` for
   human-readable output (also the default when attached to a terminal).

Symptom → cause:

| Symptom                                | Likely cause                                                                                                                                                                                                      |
|----------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Webhook returns 401                    | `git.webhook_secret` differs from the secret configured on the repository webhook, or is empty (an empty secret rejects everything)                                                                                |
| Webhook 200 but nothing happens        | Event isn't one Ramify handles (e.g. a PR `labeled` action, or a tag push)                                                                                                                                        |
| Environment stuck `failed`             | Apply exhausted its 5 attempts — read `ramifyd` logs for which provider failed, including a `readiness check` / `readiness timed out` error if the container/pod never became healthy                           |
| `ramifyd` won't start: `deploy.ssh_known_hosts_path (or explicit ...)` | Compose requires a pinned host key by default — set `ssh_known_hosts_path` or, only if you understand the tradeoff, `ssh_insecure_skip_host_key_verify: true`                                    |
| `ramifyd` won't start: `server.tcp_tls_cert_file and ...` | `server.tcp_addr` is non-loopback without TLS — add `tcp_tls_cert_file`/`tcp_tls_key_file`, bind loopback and put a reverse proxy in front, or set `tcp_insecure_allow_remote: true`             |
| Browser shows the wrong cert on a brand-new Kubernetes preview | Expected for a few seconds: the Ingress names its TLS secret before `InstallCertificate` creates it                                                                             |
| Kubernetes deploy fails with `namespaces "ramify" not found` | Ramify does not create the namespace — create it first, or point `deploy.kubernetes_namespace` at an existing one                                                       |
| Deploy fails pulling the image         | CI hasn't pushed an image for that commit SHA, or `${IMAGE_TAG}` isn't interpolated into a full image ref                                                                                                         |
| DNS resolves but the wrong app answers | No per-hostname reverse proxy on the VPS — Ramify only points DNS at `dns_target`                                                                                                                                 |
| Cert issuance fails                    | The DNS credential lacks record-write scope (doctor can't detect this without a mutating call), or Let's Encrypt rate limits — test against `--acme-ca-dir-url https://acme-staging-v02.api.letsencrypt.org/directory` |
| Environment vanished overnight         | TTL expired and the reaper swept it — raise `reaper.default_ttl` or pin it                                                                                                                                        |
| `ramify` can't reach the daemon        | Wrong `--socket` path, or `ramifyd` isn't running; for TCP you need both `--addr` and `--token`                                                                                                                   |

## Gotchas

- `ramify install` creates **directories**, not binaries. The binaries come from
  `scripts/install.sh` / `install.ps1`, a release archive, or `go build`.
- `ramify init` fails rather than writing an incomplete file — it validates
  first, so a missing required flag surfaces as `generated config is incomplete`.
- `--acme-ca-dir-url` defaults to **production** Let's Encrypt. Point it at
  staging while testing or you'll burn rate limits on failed attempts.
- `doctor`'s ACME check only confirms the directory URL responds; it
  deliberately doesn't register an account, since that would leave an unused
  ACME account behind on every run.

## The dashboard

`ramifyd` serves a single-page operational dashboard at `/dashboard/` on the TCP
listener, embedded in the binary with `go:embed` (so editing the HTML requires a
rebuild before the change appears). It lists environments with live status,
filters and search, an event timeline, follow-tail logs, and the sleep / wake /
destroy actions. It is a view onto the same control API the CLI uses, not a
second source of truth.

Two things to know before exposing it:

- **The page itself is unauthenticated**, and deliberately: a browser cannot
  attach an `Authorization` header to a top-level navigation, so gating the HTML
  would make it unreachable. Only the static shell and `/dashboard/config` (which
  returns the base domain) are exempt. Every route that reads or changes an
  environment still requires the bearer token, which the page prompts for and
  sends on its XHRs. Put `server.tcp_addr` on a trusted network regardless.
- **It needs `server.tcp_addr` and `server.tcp_token` set.** With only the unix
  socket configured there is nothing for a browser to connect to.
- **The token is kept in the browser's `localStorage`.** The transport
  hardening above (TLS on a non-loopback `tcp_addr`, plus a `Content-Security-Policy`
  applied to every response — `default-src 'self'`, `connect-src 'self'`, and no
  inline frames) is the mitigation for that: it stops the token leaking over the
  wire or exfiltrating to a third-party origin even under a script-injection
  scenario. It does not change what a script running on the page itself could
  read — no storage API does.

## Scope

Built-in providers: GitHub, GitLab, and Bitbucket (webhooks + PR/MR comments);
Compose over SSH and Kubernetes via kubectl; Cloudflare, Route 53, Google Cloud,
and DigitalOcean DNS; ACME/Let's Encrypt via DNS-01. `providers/notify/prcomment`
is the one built-in `NotifierProvider` — it's generic over any `GitProvider`
(`CommentOnPR`/`UpsertPreviewComment`), so it already posts comments on GitHub,
GitLab, or Bitbucket, whichever `git:` is configured; it isn't GitHub-specific
despite older docs/history saying "githubcomment". Adding another provider means
implementing one of the five interfaces in `providers/providerapi` and wiring it
into `ramifyd` startup. `test/contract` has shared contract suites for all five
interfaces (`deploy.go`, `dns.go`, `git.go`, `cert.go`, `notify.go`) — a new
implementation should be added to its suite rather than only unit-tested.
`RunCertificateProviderContract` only runs against a real account
(`test/e2e/cert_contract_test.go`, against Pebble) since ACME has no fake seam;
`RunNotifierProviderContract` is deliberately thin — only "a well-formed event
delivers without error" — since template defaults and no-op-on-zero-PR behavior
are implementation choices, not part of the interface.

Beyond the five required interfaces, a `DeployProvider` can optionally
implement `providerapi.CertificateInstaller`, `providerapi.CertificateRemover`,
and/or `providerapi.LogFetcher` (`providers/providerapi/capabilities.go`) —
named interfaces the reconciler and control API check for via type assertion,
rather than requiring every deploy target to support certificate install/log
retrieval directly. Compose and Kubernetes both implement all three. If
`deploy.certificate_dir` is set but the configured deploy provider doesn't
implement `CertificateInstaller`, `ramifyd` fails at startup instead of
silently never installing a certificate.

Still deliberately out of scope: image building, per-hostname routing,
idle-detection driving automatic sleep, *evicting* an environment to make room
at the concurrency ceiling (Ramify rejects instead), out-of-process plugins,
notifiers beyond PR/MR comments (e.g. Slack/Discord), and any hosted component.

## Development

```sh
go build ./... && go vet -tags=e2e ./... && golangci-lint run --build-tags=e2e && go test -race -cover ./...
```

All four must pass before a PR. `CONTRIBUTING.md` has the commit style.
