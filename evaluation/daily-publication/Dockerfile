# syntax=docker/dockerfile:1.7
FROM golang:1.25-bookworm AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -z "$(gofmt -l evaluation/daily-publication evaluation/cmd/v4-offline)" \
    && go test ./evaluation/daily-publication/cmd/phase ./evaluation/cmd/v4-offline \
    && CGO_ENABLED=1 go build -trimpath -o /out/v4-offline ./evaluation/cmd/v4-offline \
    && CGO_ENABLED=1 go build -trimpath -o /out/daily-publication-phase ./evaluation/daily-publication/cmd/phase

FROM debian:bookworm-slim AS phase
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/v4-offline /usr/local/bin/v4-offline
COPY --from=build /out/daily-publication-phase /usr/local/bin/daily-publication-phase
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/daily-publication-phase"]
