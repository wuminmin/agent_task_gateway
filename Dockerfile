# syntax=docker/dockerfile:1.7
FROM golang:1.25-bookworm AS base
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM base AS verify
RUN test -z "$(gofmt -l .)"
RUN go vet ./...
RUN go test -race ./...

FROM base AS build
ARG TARGET=gateway
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${TARGET}

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 taskbound \
    && useradd --system --uid 10001 --gid taskbound --home-dir /nonexistent --shell /usr/sbin/nologin taskbound \
    && mkdir -p /data/snapshot-index \
    && chown -R 10001:10001 /data
COPY --from=build /out/app /usr/local/bin/app
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/app"]
