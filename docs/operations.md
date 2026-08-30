# Operations Guide

## Health and readiness

`/healthz` verifies that the daemon can read the state store. `/readyz` verifies
that the event store is available for reconciliation, that the durable event
worker's poll loop has completed an iteration within the last 30s (catching a
hung or panicked worker that would otherwise look alive), and that the pending
inbox backlog is under a threshold — so orchestration stops routing traffic to a
daemon that is running but not actually reconciling. `/metrics` exposes counters
in Prometheus text format, including webhook deliveries, duplicates, retries,
reconciliation failures, cleanup failures, pending inbox work, dead-lettered
events, and the worker's last heartbeat time.

Keep these endpoints behind the same local socket or authenticated/TLS-protected
TCP listener as the control API.

## Database backups

Create a consistent SQLite backup with:

```sh
ramify backup --config /etc/ramify/ramify.yaml \
  --output /var/backups/ramify/ramify-$(date +%Y%m%d-%H%M%S).db
```

Backups are created with SQLite's `VACUUM INTO` mechanism and are written with
restrictive permissions where the operating system supports them. The destination
must not already exist.

Test restores regularly by opening a copy with `ramifyd` or the SQLite CLI before
depending on the backup for disaster recovery.

## Readiness before "ready"

A successful `Apply` from the deploy provider is not, by itself, enough to
mark an environment `ready`. Before DNS and the certificate are touched, the
reconciler polls the provider's `HealthCheck` — every
`deploy.readiness_poll_interval` (default 2s) — until it reports healthy or
`deploy.readiness_timeout` (default 2m) elapses. A readiness timeout surfaces
as an ordinary apply failure and goes through the same 5-attempt retry budget
as any other provider error, so it can dead-letter like any other permanent
failure rather than leaving a half-up environment announced as live.

## Durable event processing

Webhook deliveries are stored before acknowledgement and deduplicated by the
delivery ID the configured Git host sends: `X-GitHub-Delivery` for GitHub,
`X-Gitlab-Event-UUID` for GitLab, `X-Hook-UUID` for Bitbucket. A delivery that
arrives without one is rejected, since it cannot be deduplicated on redelivery. Failed events record an attempt count, next retry time, and
last error. The daemon's event worker retries due work continuously, replaying
up to `reaper.event_concurrency` events in parallel (default 8) — a bound
that keeps one slow provider call from stalling every other pending event
while still serializing events for the same project/branch.

An event remaining pending indicates that the desired state has not been confirmed.
Inspect logs and `/metrics` before manually changing provider resources.

Retry delays use exponential backoff with jitter, so a batch of events that failed
against the same provider outage does not retry in lockstep when it recovers.

## Dead-lettered events

An event is retired instead of retried when it fails permanently — an unmanaged
DNS record collision, an ownership mismatch, a payload that cannot be parsed, an
event whose environment no longer exists — or when it exhausts its attempt
budget. Retired events keep their last error and stay in the `events` table for
inspection, but leave the pending and due sets so the worker is never blocked by
work that cannot succeed.

`ramify_events_dead_lettered` is the gauge to alert on: a non-zero value means
work was dropped and a preview environment may not match its branch. Investigate
the recorded `last_error`, resolve the underlying cause (for example, remove the
conflicting DNS record), and re-trigger the deployment by pushing to the branch.

Transient failures — timeouts, rate limits, 5xx responses — are never
dead-lettered on the first failure; only errors a provider explicitly marks
permanent are.

## Paginated listings

`GET /environments/` returns at most 100 environments by default and 500 at most.
Pass `limit` and `offset` to page through larger installations. When more rows
remain, the response carries an `X-Ramify-Next-Offset` header holding the offset
of the next page; its absence means the last page was reached. The `ramify list`
command follows these headers automatically and still prints every environment.

## The operational dashboard

Setting `server.tcp_addr` also serves a dashboard at `/dashboard/` on that
listener. It is embedded in the `ramifyd` binary with no external assets, so it
needs no separate deployment and works on an air-gapped network.

It shows the same data `ramify list` and `ramify status` return — status, TTL
countdown, preview host, artifact reference, deploy ref — and performs the same
sleep, wake, and destroy calls against the same control API. Deployment logs are
tailed automatically, pausing while you scroll back through them. Destroy requires
the branch name typed back before it will run.

It authenticates with the `server.tcp_token` value, entered in the browser and
held in that browser's local storage only. Polling stops while the tab is hidden
and backs off geometrically when the daemon is unreachable, so a dashboard left
open overnight is not a load source.

Every response from the control API — dashboard included — carries a defensive
header set (`X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer`, and a `Content-Security-Policy` scoped to
`'self'`). Combined with TLS on a non-loopback `tcp_addr` (below), this is the
mitigation for the token living in local storage: it stops the token leaking
over the wire or exfiltrating to a third-party origin even under a
script-injection scenario, though it doesn't change what a script running on
the page itself could read.

## Limiting which branches deploy

Left alone, every branch push produces an environment holding a DNS record, a
certificate, and a running container until its TTL lapses. The `filter:` block
bounds that; omitting it keeps the original behavior.

```yaml
filter:
  pr_only: true
  deny_branches: ["dependabot/**", "renovate/**"]
  required_labels: ["preview"]
  max_concurrent_envs: 25
```

Operationally, three things are worth knowing.

Patterns use the GitHub Actions convention: `*` stops at a slash, `**` does not.
`dependabot/*` will not stop `dependabot/npm/lodash` — write `dependabot/**`.

`required_labels` is skipped where the host cannot report labels. Bitbucket
Cloud has no pull request labels, and a branch push has no request to carry
them, so the gate does nothing there rather than blocking everything. Pair it
with `pr_only: true` if you need it absolute.

`max_concurrent_envs` rejects rather than evicts. Reaching the ceiling never
tears down a live preview, and updates to environments that already exist keep
deploying — so nothing freezes on a stale commit. A slot frees when an
environment is destroyed or expires. Every status except `destroyed` counts,
`failed` and `pending` included, since both can hold partially created
infrastructure.

Skipped events are logged, not commented on the pull request: a denied pattern
matches on every push to that branch, and a comment each time would be noise for
a standing operator decision. Grep for the reason:

```sh
journalctl -u ramifyd | grep "skipped by admission policy"
```

## Production security baseline

- Configure `deploy.ssh_known_hosts_path`. This is enforced, not just
  recommended: `ramifyd` refuses to start a Compose deployment without it
  unless you explicitly set `deploy.ssh_insecure_skip_host_key_verify: true`.
- Keep the Unix socket at mode `0660` with a dedicated group. On startup,
  `ramifyd` refuses to remove and rebind a socket path that another process is
  actively serving, and (on Linux/macOS) holds an advisory lock on a sibling
  `<socket_path>.lock` file for its lifetime, so a second daemon accidentally
  started against the same `server.socket_path` cannot disconnect or hijack
  the running one.
- Require `server.tcp_token` whenever `server.tcp_addr` is configured.
- Put remote TCP access behind TLS: set `server.tcp_tls_cert_file` and
  `server.tcp_tls_key_file` (or mTLS via your own reverse proxy). This is also
  enforced — a non-loopback `tcp_addr` without TLS refuses to start unless you
  explicitly set `server.tcp_insecure_allow_remote: true`.
- Use zone-scoped DNS tokens and repository-scoped Git credentials, whichever providers you configure.
- Protect the SQLite database and ACME storage directory with mode `0700`/`0600`.
- Treat `/dashboard/` as public. The dashboard page and the base domain it reads
  are served without a bearer token, because a browser cannot attach an
  `Authorization` header to a top-level navigation. No environment data sits
  behind that exemption, but the listener itself belongs on a trusted network.
- Destroying an environment removes its installed TLS private key material too
  (the Kubernetes TLS Secret, or the Compose certificate/key files under
  `deploy.certificate_dir`), not just its DNS record and ACME certificate.
