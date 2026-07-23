# syntax=docker/dockerfile:1.7
FROM golang:1.25-bookworm AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -z "$(gofmt -l evaluation)" \
    && go test ./evaluation/... \
    && CGO_ENABLED=1 go build -trimpath -o /out/eval-runner ./evaluation/cmd/runner \
    && CGO_ENABLED=1 go build -trimpath -o /out/ast-gateway ./evaluation/cmd/ast-gateway \
    && CGO_ENABLED=1 go build -trimpath -o /out/exposure-eval ./evaluation/cmd/exposure-eval

FROM debian:bookworm-slim AS runner
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/eval-runner /usr/local/bin/eval-runner
COPY --from=build /out/ast-gateway /usr/local/bin/ast-gateway
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/eval-runner"]

FROM build AS fuzz
RUN apt-get update \
    && apt-get install -y --no-install-recommends time \
    && rm -rf /var/lib/apt/lists/*
RUN go test -c -run '^$' -fuzz '^FuzzAuthorizeNeverPanics$' \
    -o /usr/local/bin/taskgate-fuzz.test ./evaluation/fuzz
WORKDIR /src

FROM debian:bookworm-slim AS exposure
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/exposure-eval /usr/local/bin/exposure-eval
ENTRYPOINT ["/usr/local/bin/exposure-eval"]

FROM python:3.12.4-slim-bookworm AS artifacts
WORKDIR /workspace
ENTRYPOINT ["python3", "/workspace/evaluation/plots/generate.py"]
