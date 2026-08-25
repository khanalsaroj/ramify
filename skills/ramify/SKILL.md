---
name: ramify
description: How to set up, configure, operate, and troubleshoot Ramify — the self-hosted preview-environment control plane in this repo (the `ramify` CLI and `ramifyd` daemon). Use when working with ramify.yaml, the ramify CLI, the GitHub webhook, the Compose deploy target, DNS/TLS lifecycle, TTL expiry, or when diagnosing why a preview environment did not come up.
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

## Read this first — two things Ramify does NOT do

These cause most of the confusion, and neither is a bug:

1. **Ramify never builds images.** The reconciler's contract is explicit:
   `DeployProvider.Apply` only ever receives an already-built artifact ref. That
   ref is the **head commit SHA** from the webhook. Your CI must build and push
   an image for that SHA *before* Ramify deploys it.
2. **Ramify does not route traffic per hostname.** It points DNS at a single
   address (`deploy.dns_target`) and brings the container up. Getting
   `feature-x.preview.example.com` to the right container is your reverse
   proxy's job — Traefik or Caddy with Docker labels on the Compose service.

## The Compose contract

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

Then add the GitHub webhook: URL `https://your-host/webhooks/github`, content
type JSON, secret from step 5, events **Pull requests** + **Pushes**.

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

github:
  token: $RAMIFY_GITHUB_TOKEN        # only needed to post PR comments
  webhook_secret: $RAMIFY_GITHUB_WEBHOOK_SECRET

deploy:
  ssh_addr: deploy-host.example.com:22
  ssh_user: ramify                   # default
  ssh_private_key_path: /etc/ramify/deploy_key
  ssh_known_hosts_path: /etc/ramify/known_hosts
  compose_file: /srv/ramify/docker-compose.yml
  dns_target: 203.0.113.10

dns:
  zone: preview.example.com
  cloudflare_api_token: $RAMIFY_CLOUDFLARE_API_TOKEN

acme:
  email: ops@example.com
  ca_dir_url: https://acme-v02.api.letsencrypt.org/directory
```

**Required at startup** — `ramifyd` refuses to boot without all of these:
`base_domain`, `store.path`, `github.webhook_secret`, `deploy.ssh_addr`,
`deploy.compose_file`, `dns.zone`, `acme.email`.

Annotated reference: `ramify.example.yaml`.

## CLI reference

| Command                   | Key flags                                                                                                                                         | Does                                                                         |
|---------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------|
| `ramify install`          | `--config-dir`, `--data-dir`                                                                                                                      | Creates config/data dirs. Does **not** install binaries.                     |
| `ramify init`             | `--output`, `--base-domain`, `--github-*`, `--deploy-*`, `--dns-zone`, `--cloudflare-token`, `--acme-email`, `--default-ttl`, `--acme-ca-dir-url` | Writes `ramify.yaml` (mode 0600) non-interactively; validates before writing |
| `ramify list`             | `--project owner/repo`                                                                                                                            | Table: ID, PROJECT, BRANCH, STATUS, SUBDOMAIN, ARTIFACT                      |
| `ramify status <branch>`  | `--project`                                                                                                                                       | Full detail for one environment                                              |
| `ramify logs <branch>`    | `--project`                                                                                                                                       | Container logs (last 500 lines, via Compose)                                 |
| `ramify destroy <branch>` | `--project`, `-y`/`--yes`                                                                                                                         | Manual teardown; confirms unless `-y`                                        |
| `ramify doctor`           | `--config`                                                                                                                                        | Independently checks config, Cloudflare, SSH, webhook secret, ACME           |

Global flags: `--socket`, `--addr`, `--token`. Per-command detail:
`ramify <cmd> --help`.

Branch-taking commands resolve `<branch>` to exactly one environment. If the
same branch name exists in several projects the command fails with
`multiple environments match branch "x"; pass --project to disambiguate`.

## Lifecycle

Statuses: `pending` → `deploying` → `ready`, or `failed`. Teardown runs
`destroying` → `destroyed`. (`sleeping` exists in the store and the control API
exposes `/sleep` and `/wake`, but automatic idle-detection is not implemented —
see `DECISIONS.md` → Deferred.)

Webhook events map to actions:

| GitHub event                                   | Ramify action            |
|------------------------------------------------|--------------------------|
| `pull_request` opened / reopened / synchronize | Apply (create or update) |
| `push` to a branch                             | Apply                    |
| `pull_request` closed                          | Destroy                  |
| branch deleted                                 | Destroy                  |

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

1. **`ramify doctor` first.** It isolates each dependency and names the failure:
   Cloudflare token resolving the zone, SSH reachability + auth, webhook secret
   length (must be ≥ 16 chars), ACME directory reachability. Note it dials SSH
   with host-key verification disabled — that check is connectivity only, and it
   warns if `ssh_known_hosts_path` is unset, which *does* matter in production.
2. **`ramify logs <branch>`** for the container's own output.
3. **`ramifyd` logs are structured JSON.** Set `RAMIFY_LOG_FORMAT=text` for
   human-readable output (also the default when attached to a terminal).

Symptom → cause:

| Symptom                                | Likely cause                                                                                                                                                                                                      |
|----------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Webhook returns 401                    | `github.webhook_secret` differs from the secret in GitHub's webhook config                                                                                                                                        |
| Webhook 200 but nothing happens        | Event isn't one Ramify handles (e.g. a PR `labeled` action, or a tag push)                                                                                                                                        |
| Environment stuck `failed`             | Apply exhausted its 5 attempts — read `ramifyd` logs for which provider failed                                                                                                                                    |
| Deploy fails pulling the image         | CI hasn't pushed an image for that commit SHA, or `${IMAGE_TAG}` isn't interpolated into a full image ref                                                                                                         |
| DNS resolves but the wrong app answers | No per-hostname reverse proxy on the VPS — Ramify only points DNS at `dns_target`                                                                                                                                 |
| Cert issuance fails                    | Cloudflare token lacks zone-edit scope (doctor can't detect this without a mutating call), or Let's Encrypt rate limits — test against `--acme-ca-dir-url https://acme-staging-v02.api.letsencrypt.org/directory` |
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

## Scope

Built-in providers: GitHub (webhooks + PR comments), Compose over SSH,
Cloudflare DNS, and ACME/Let's Encrypt via DNS-01. Adding another means
implementing one of the five interfaces in `providers/providerapi` and wiring it
into `ramifyd` startup; the shared contract suites in `test/contract` pin the
behavior any implementation must satisfy. Deliberately out of scope for now:
Kubernetes, GitLab and Bitbucket, non-Cloudflare DNS, idle-sleep, a web
dashboard, and out-of-process plugins — see `DECISIONS.md`.

## Development

```sh
go build ./... && go vet ./... && golangci-lint run && go test -race -cover ./...
```

All four must pass before a PR. `CONTRIBUTING.md` has the commit style.
