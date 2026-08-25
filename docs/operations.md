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

Webhook deliveries are stored before acknowledgement and deduplicated by
`X-GitHub-Delivery`. Failed events record an attempt count, next retry time, and
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

## Production security baseline

- Configure `deploy.ssh_known_hosts_path`.
- Keep the Unix socket at mode `0660` with a dedicated group.
- Require `server.tcp_token` whenever `server.tcp_addr` is configured.
- Put remote TCP access behind TLS or mTLS.
- Use zone-scoped Cloudflare tokens and repository-scoped GitHub credentials.
- Protect the SQLite database and ACME storage directory with mode `0700`/`0600`.
