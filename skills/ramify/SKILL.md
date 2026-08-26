---
name: ramify
description: How to set up, configure, operate, and troubleshoot Ramify — the self-hosted preview-environment control plane in this repo (the `ramify` CLI and `ramifyd` daemon). Use when working with ramify.yaml, the ramify CLI, a GitHub/GitLab/Bitbucket webhook, the Compose or Kubernetes deploy target, the Cloudflare/Route 53/Google Cloud/DigitalOcean DNS providers, the operational dashboard, DNS/TLS lifecycle, TTL expiry, or when diagnosing why a preview environment did not come up.
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
`docs/providers.md`.

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
store:
  path: /var/lib/ramify/ramify.db

reaper:
  interval: 5m                       # how often expiry is enforced
  default_ttl: 72h                   # refreshed on every successful apply

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
  compose_file: /srv/ramify/docker-compose.yml
  dns_target: 203.0.113.10

dns:
  provider: cloudflare               # cloudflare | route53 | googlecloud | digitalocean
  zone: preview.example.com
  cloudflare_api_token: $RAMIFY_CLOUDFLARE_API_TOKEN

acme:
  email: ops@example.com
  ca_dir_url: https://acme-v02.api.letsencrypt.org/directory
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
| `deploy.provider: compose`                   | `deploy.ssh_addr`, `deploy.compose_file`, `deploy.ssh_private_key_path`  |
| `deploy.provider: kubernetes`                | `deploy.kubernetes_namespace`                                           |
| `dns.provider: cloudflare` or `digitalocean` | `dns.api_token` (Cloudflare also accepts `dns.cloudflare_api_token`)     |
| `dns.provider: googlecloud`                  | `dns.project`, `dns.zone_id`                                            |
| `server.tcp_addr` set                        | `server.tcp_token`                                                      |

Route 53 and Google Cloud DNS need no token in the file: they use the AWS SDK
credential chain and Application Default Credentials respectively. For Route 53
`dns.zone_id` is optional — left empty, the hosted zone is resolved by name.

Annotated reference: `ramify.example.yaml`.

## CLI reference

| Command                   | Key flags                                                                                                                                         | Does                                                                         |
|---------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------|
| `ramify install`          | `--config-dir`, `--data-dir`                                                                                                                      | Creates config/data dirs. Does **not** install binaries.                     |
| `ramify init`             | `--output`, `--base-domain`, `--git-provider`, `--github-*`, `--deploy-provider`, `--deploy-*`, `--kubernetes-*`, `--dns-provider`, `--dns-zone`, `--cloudflare-token`, `--dns-token`, `--acme-email` | Writes `ramify.yaml` (mode 0600) non-interactively; validates before writing |
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

## Lifecycle

Statuses: `pending` → `deploying` → `ready`, or `failed`. Teardown runs
`destroying` → `destroyed`. A `ready` environment can also go `sleeping` and
back. The store enforces this as a transition graph and rejects any jump that
isn't an edge in it, so a test or a caller cannot move an environment straight
from `pending` to `ready`.

Sleep and wake are operator-driven only: `/sleep` and `/wake` are exposed on the
control API and the dashboard, but **automatic idle-detection is not
implemented** (see `DECISIONS.md` → Deferred). Only the Kubernetes deploy
provider can act on them.

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
deploy/DNS/cert sequence up to **5 times** before marking the environment
`failed`.

**Crash safety:** every event is written to the store *before* any provider is
called, and the webhook returns 202 immediately while work proceeds
asynchronously. On restart `ramifyd` replays unprocessed events, so a mid-flight
crash resumes rather than losing work. Every provider operation is idempotent.

**TTL:** each successful apply sets `ttl_expires_at = now + default_ttl`, so an
actively-pushed branch keeps renewing and only expires `default_ttl` after the
last push. The reaper sweeps on `reaper.interval`. Environments flagged `pinned`
are never swept regardless of TTL. `default_ttl: 0` disables expiry entirely.

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
   with host-key verification disabled — that check is connectivity only, and it
   warns if `ssh_known_hosts_path` is unset, which *does* matter in production.
2. **`ramify logs <branch>`** for the container's own output.
3. **`ramifyd` logs are structured JSON.** Set `RAMIFY_LOG_FORMAT=text` for
   human-readable output (also the default when attached to a terminal).

Symptom → cause:

| Symptom                                | Likely cause                                                                                                                                                                                                      |
|----------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Webhook returns 401                    | `git.webhook_secret` differs from the secret configured on the repository webhook, or is empty (an empty secret rejects everything)                                                                                |
| Webhook 200 but nothing happens        | Event isn't one Ramify handles (e.g. a PR `labeled` action, or a tag push)                                                                                                                                        |
| Environment stuck `failed`             | Apply exhausted its 5 attempts — read `ramifyd` logs for which provider failed                                                                                                                                    |
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

## Scope

Built-in providers: GitHub, GitLab, and Bitbucket (webhooks + PR comments);
Compose over SSH and Kubernetes via kubectl; Cloudflare, Route 53, Google Cloud,
and DigitalOcean DNS; ACME/Let's Encrypt via DNS-01. Adding another means
implementing one of the five interfaces in `providers/providerapi` and wiring it
into `ramifyd` startup; the shared contract suites in `test/contract` pin the
behavior any implementation must satisfy, and a new provider should be added to
its suite rather than only unit-tested.

Still deliberately out of scope: image building, per-hostname routing,
idle-detection driving automatic sleep, `max_concurrent_envs` eviction,
out-of-process plugins, notifiers beyond PR comments, and any hosted component
— see `DECISIONS.md`.

## Development

```sh
go build ./... && go vet ./... && golangci-lint run && go test -race -cover ./...
```

All four must pass before a PR. `CONTRIBUTING.md` has the commit style.
