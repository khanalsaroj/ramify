# The Go version must satisfy go.mod's `go` directive: the official golang images
# set GOTOOLCHAIN=local, so an older base fails outright rather than downloading a
# newer toolchain.
FROM golang:1.25-alpine

# Pebble's image is FROM scratch and has no shell, so it cannot copy its own test
# root CA onto a shared volume. Lift it out at build time instead; the ACME client
# in test/e2e needs it to trust Pebble's HTTPS endpoint.
COPY --from=ghcr.io/letsencrypt/pebble:2.10.1 /test/certs/pebble.minica.pem /shared/pebble.minica.pem

RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
CMD ["go", "test", "./test/e2e/...", "-tags=e2e", "-run", "TestFullLifecycle", "-v", "-timeout=10m"]
