# Quickstart

This walks through taking Ramify from zero to a working preview-environment setup
against a real GitHub repository, a real Cloudflare-managed DNS zone, and one VPS
you already control. Every `ramify`/`ramifyd` command below was run against a real
generated config during this project's build — the commands work as written. The
handful of steps that create external accounts or provision infrastructure (a VPS,
a Cloudflare zone, a GitHub webhook) are things only you can do; they're described
precisely so nothing is left assumed.

## Prerequisites

- A GitHub repository you can add a webhook to.
- A domain (or subdomain) whose DNS is managed by Cloudflare, e.g. `preview.example.com`.
- One VPS you control, reachable over SSH, with Docker and the Compose plugin
  installed (`docker compose version` should work on it).
- Either the installer below, or Go 1.23+ locally to build the two Ramify
  binaries from source.

## 1. Provision the pieces Ramify doesn't manage

Ramify deploys to infrastructure you already operate — it doesn't provision any of
this for you.

1. **VPS**: any host with Docker + the Compose plugin. Note its public IP —
   you'll need it twice (SSH target and DNS target).
2. **Cloudflare zone**: add `preview.example.com` (or whatever subdomain you want
   preview environments under) as a zone in Cloudflare, or as a subdomain
   delegated to a zone you already manage there.
3. **Cloudflare API token**: create one (My Profile → API Tokens → Create Token)
   scoped to `Zone:DNS:Edit` for that zone only.
4. **GitHub token**: a personal access token (or GitHub App installation token)
   with permission to comment on pull requests in your repository.

## 2. Install `ramify` and `ramifyd`

```sh
curl -fsSL https://raw.githubusercontent.com/khanalsaroj/ramify/main/scripts/install.sh | bash
```

Installs both binaries to `/usr/local/bin` (falls back to `~/.local/bin`).
Windows: `iwr -useb https://raw.githubusercontent.com/khanalsaroj/ramify/main/scripts/install.ps1 | iex`
in PowerShell. Pin a version with `RAMIFY_VERSION=v0.3.1` (or `$env:RAMIFY_VERSION`
on Windows), or build from source instead:

```sh
git clone https://github.com/khanalsaroj/ramify.git
cd ramify
go build -o ramify   ./cmd/ramify
go build -o ramifyd  ./cmd/ramifyd
sudo mv ramify ramifyd /usr/local/bin/
```

## 3. Create Ramify's directories

```sh
ramify install --config-dir /etc/ramify --data-dir /var/lib/ramify
```

This just creates the two directories and tells you what's next — it doesn't
write any config yet.

## 4. Generate a deploy SSH key

Ramify SSHes to your VPS to run `docker compose`. Generate a dedicated key rather
than reusing your personal one:

```sh
ssh-keygen -t ed25519 -N "" -f /etc/ramify/deploy_key
ssh-copy-id -i /etc/ramify/deploy_key.pub ramify@YOUR_VPS_IP
```

(If the `ramify` user doesn't exist on the VPS yet, create it first and add it to
the `docker` group so it can run Compose: `sudo useradd -m -G docker ramify`.)

Pin the host key so Ramify verifies it on every connection, instead of trusting it
blindly:

```sh
ssh-keyscan -t ed25519 YOUR_VPS_IP >> /etc/ramify/known_hosts
```

## 5. Put a Compose file on the VPS

Ramify runs `docker compose -f <path> up -d` with `IMAGE_TAG` and
`COMPOSE_PROJECT_NAME` set per environment — write the Compose file that
describes how your app runs, e.g. `/srv/ramify/docker-compose.yml` on the VPS.

`IMAGE_TAG` is the head commit SHA of the branch being deployed, not a full
image reference, so interpolate it into one your registry can resolve. Ramify
never builds the image — your CI must have pushed it for that SHA already:

```yaml
services:
  app:
    image: ghcr.io/OWNER/REPO:${IMAGE_TAG}
    restart: unless-stopped
    # expose whatever port your reverse proxy on the VPS routes to; Ramify only
    # points DNS at the VPS's own address (deploy.dns_target below) — routing
    # each environment's traffic to the right container by hostname is your
    # reverse proxy's job (e.g. Traefik or Caddy with Docker labels on this
    # service).
```

## 6. Generate `ramify.yaml`

```sh
export RAMIFY_GITHUB_TOKEN=ghp_your_token_here
export RAMIFY_GITHUB_WEBHOOK_SECRET=$(openssl rand -hex 32)
export RAMIFY_CLOUDFLARE_API_TOKEN=your_cloudflare_token_here

ramify init \
  --output /etc/ramify/ramify.yaml \
  --base-domain preview.example.com \
  --store-path /var/lib/ramify/ramify.db \
  --github-token '$RAMIFY_GITHUB_TOKEN' \
  --github-webhook-secret '$RAMIFY_GITHUB_WEBHOOK_SECRET' \
  --deploy-ssh-addr YOUR_VPS_IP:22 \
  --deploy-ssh-user ramify \
  --deploy-ssh-key /etc/ramify/deploy_key \
  --deploy-ssh-known-hosts /etc/ramify/known_hosts \
  --deploy-compose-file /srv/ramify/docker-compose.yml \
  --deploy-dns-target YOUR_VPS_IP \
  --dns-zone preview.example.com \
  --cloudflare-token '$RAMIFY_CLOUDFLARE_API_TOKEN' \
  --acme-email you@example.com
```

Note the single quotes around the `$VAR` flag values: passed literally as
`$RAMIFY_GITHUB_TOKEN`, Ramify resolves that reference from the environment every
time it loads the config, rather than baking the secret into the file on disk. Run
`echo $RAMIFY_GITHUB_WEBHOOK_SECRET` and save that value — you'll paste it into
GitHub's webhook settings in step 9.

## 7. Validate everything before starting the daemon

```sh
ramify doctor --config /etc/ramify/ramify.yaml
```

This checks, independently: the config file parses and has every required field;
your Cloudflare token can resolve the configured zone; the deploy host is
reachable and the SSH key authenticates; the webhook secret is set; and the ACME
directory URL is reachable. A `[FAIL]` line names exactly which check failed and
why — fix that one thing and re-run rather than guessing.

## 8. Run the daemon

For a first run, in the foreground:

```sh
sudo ramifyd --config /etc/ramify/ramify.yaml
```

You should see structured JSON logs: config loaded (with secret values redacted,
only which fields were set), the control API starting on
`/var/run/ramify/ramify.sock`, and the reaper loop beginning. Once you're satisfied
it's healthy, run it as a systemd service instead:

```ini
# /etc/systemd/system/ramifyd.service
[Unit]
Description=Ramify control plane
After=network-online.target

[Service]
ExecStart=/usr/local/bin/ramifyd --config /etc/ramify/ramify.yaml
Restart=on-failure
User=root

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now ramifyd
```

`ramifyd` needs to be reachable from GitHub's webhook delivery servers on the
internet. Put a reverse proxy with TLS in front of it (it listens on a unix
socket by default; set `server.tcp_addr`/`server.tcp_token` in `ramify.yaml` if
you'd rather expose TCP directly) and forward `/webhooks/github` through to it —
Ramify does not include its own public-facing reverse proxy or TLS termination
for its control API.

## 9. Add the GitHub webhook

In your repository: Settings → Webhooks → Add webhook.

- **Payload URL**: `https://your-ramifyd-host/webhooks/github`
- **Content type**: `application/json`
- **Secret**: the value of `$RAMIFY_GITHUB_WEBHOOK_SECRET` from step 6
- **Events**: "Pull requests" and "Pushes" (or "Send me everything" — Ramify
  ignores event kinds it doesn't handle)

## 10. Open a pull request

Push a branch and open a PR. Within a few seconds you should see:

```sh
ramify status your-branch-name
```

report `Status: ready`, a comment appear on the PR with the preview URL, and
`https://your-branch-name.preview.example.com` resolve and serve TLS. Closing the
PR tears the environment down automatically; `ramify destroy your-branch-name`
does the same thing manually, and `ramify list` shows everything currently
running.

## Troubleshooting

- `ramify doctor` first — it's designed to isolate exactly which piece is broken.
- `ramify logs your-branch-name` prints the deployed container's logs (requires
  the Compose deploy provider, which is the only one Ramify ships).
- Every log line from `ramifyd` is structured JSON; `RAMIFY_LOG_FORMAT=text` (or
  running it attached to a terminal) switches to human-readable text instead.
