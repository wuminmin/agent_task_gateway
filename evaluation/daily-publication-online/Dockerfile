# syntax=docker/dockerfile:1.7
FROM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -z "$(gofmt -l evaluation/cmd/rq5-online-transition cmd/snapshot-sidecar-install)" \
    && go test ./evaluation/cmd/rq5-online-transition ./cmd/snapshot-sidecar-install \
    && CGO_ENABLED=1 go build -trimpath -o /out/rq5-online-transition ./evaluation/cmd/rq5-online-transition \
    && CGO_ENABLED=1 go build -trimpath -o /out/snapshot-sidecar-install ./cmd/snapshot-sidecar-install

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/rq5-online-transition /usr/local/bin/rq5-online-transition
COPY --from=build /out/snapshot-sidecar-install /usr/local/bin/snapshot-sidecar-install
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/rq5-online-transition"]
