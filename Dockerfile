FROM golang:1.26-trixie AS builder

# Optional CA cert for MITM proxy environments (glob matches nothing if file absent)
COPY mitmproxy-ca-cert.cr[t] /usr/local/share/ca-certificates/
RUN update-ca-certificates 2>/dev/null || true

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG GIT_COMMIT=unknown

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/myers/drawbar/pkg/version.Version=${VERSION} -X github.com/myers/drawbar/pkg/version.GitCommit=${GIT_COMMIT}" \
    -o /out/controller ./cmd/controller/

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/myers/drawbar/pkg/version.Version=${VERSION} -X github.com/myers/drawbar/pkg/version.GitCommit=${GIT_COMMIT}" \
    -o /out/entrypoint ./cmd/entrypoint/

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*
RUN useradd -u 1000 -m runner
COPY --from=builder /out/controller /controller
COPY --from=builder /out/entrypoint /entrypoint
USER 1000
ENTRYPOINT ["/controller"]
