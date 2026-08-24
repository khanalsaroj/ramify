FROM golang:1.23-alpine
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
CMD ["go", "test", "./test/e2e/...", "-tags=e2e", "-run", "TestFullLifecycle", "-v", "-timeout=10m"]
