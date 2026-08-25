# Contributing to Ramify

## Development setup

- Go 1.25.6+ (matches the `go` directive in `go.mod`)
- Docker + Docker Compose (for the e2e harness under `test/e2e`)

## Workflow

```sh
go build ./...
go vet -tags=e2e ./...
golangci-lint run --build-tags=e2e
go test -race -cover ./...
```

All four must pass before opening a pull request.

## Commit style

[Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`,
`test:`, `refactor:`, `chore:`.

## Adding a provider

Every provider implements one or more interfaces in `providers/providerapi`. New
provider packages must pass the shared behavioral suite in `test/contract` — see
[`docs/providers.md`](docs/providers.md).
