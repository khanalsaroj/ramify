# Ramify

Ramify is a self-hosted, open-source control plane that turns Git branches and pull
requests into short-lived, automatically routed, automatically expiring preview
environments deployed to infrastructure you already operate — unlike Preevy, which
tunnels into environments without owning DNS/TLS lifecycle, Ramify owns the full
create-route-expire lifecycle including DNS and certificates; and unlike
Coolify/Dokploy, which are general-purpose self-hosted PaaS platforms, Ramify does one
thing only: ephemeral preview environments driven by git activity.

## Status

Early development. See [`DECISIONS.md`](DECISIONS.md) for judgment calls made during
the build and [`docs/quickstart.md`](docs/quickstart.md) to get started.

## License

Apache-2.0. See [`LICENSE`](LICENSE).
