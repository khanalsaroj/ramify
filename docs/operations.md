# Operations Guide

## Health and readiness

`/healthz` verifies that the daemon can read the state store. `/readyz` verifies
that the event store is available for reconciliation. `/metrics` exposes counters
in Prometheus text format, including webhook deliveries, duplicates, retries,
reconciliation failures, cleanup failures, pending inbox work, and dead-lettered
events.

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

## Durable event processing

Webhook deliveries are stored before acknowledgement and deduplicated by the
delivery ID the configured Git host sends: `X-GitHub-Delivery` for GitHub,
`X-Gitlab-Event-UUID` for GitLab, `X-Hook-UUID` for Bitbucket. A delivery that
arrives without one is rejected, since it cannot be deduplicated on redelivery. Failed events record an attempt count, next retry time, and
last error. The daemon's event worker retries due work continuously.

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

## Production security baseline

- Configure `deploy.ssh_known_hosts_path`.
- Keep the Unix socket at mode `0660` with a dedicated group.
- Require `server.tcp_token` whenever `server.tcp_addr` is configured.
- Put remote TCP access behind TLS or mTLS.
- Use zone-scoped DNS tokens and repository-scoped Git credentials, whichever providers you configure.
- Protect the SQLite database and ACME storage directory with mode `0700`/`0600`.
- Treat `/dashboard/` as public. The dashboard page and the base domain it reads
  are served without a bearer token, because a browser cannot attach an
  `Authorization` header to a top-level navigation. No environment data sits
  behind that exemption, but the listener itself belongs on a trusted network.
