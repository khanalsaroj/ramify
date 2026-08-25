# Operations Guide

## Health and readiness

`/healthz` verifies that the daemon can read the state store. `/readyz` verifies
that the event store is available for reconciliation. `/metrics` exposes counters
in Prometheus text format, including webhook deliveries, duplicates, retries,
reconciliation failures, cleanup failures, and pending inbox work.

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

## Production security baseline

- Configure `deploy.ssh_known_hosts_path`.
- Keep the Unix socket at mode `0660` with a dedicated group.
- Require `server.tcp_token` whenever `server.tcp_addr` is configured.
- Put remote TCP access behind TLS or mTLS.
- Use zone-scoped Cloudflare tokens and repository-scoped GitHub credentials.
- Protect the SQLite database and ACME storage directory with mode `0700`/`0600`.
