# syntax=docker/dockerfile:1.7
# Offline runner toolbox for air-gapped execution hosts.
#
# The evaluation harness runs `go run`/`go build`, make, jq, git, and docker
# compose on the host while driving the Compose stacks. An offline server has
# none of that, so this image carries the complete toolchain plus a warmed Go
# module and build cache; with GOPROXY=off every harness compile resolves from
# the baked caches and never touches the network.
#
# Build from the repository root:
#   docker build -f offline/toolbox.Dockerfile -t taskgate-toolbox:offline .
#
# Run on the server with the host Docker socket and the repo mounted at the
# SAME absolute path as on the host (Compose bind mounts resolve on the host):
#   docker run --rm -it --network host --security-opt label=disable \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -v /opt/taskgate:/opt/taskgate \
#     -w /opt/taskgate/agent_task_gateway taskgate-toolbox:offline bash
FROM golang:1.25-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl gnupg make jq git bash openssl python3 \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

# The harness preflight requires Docker Compose >= 2.24.4 (a client-side
# check); fail the build rather than ship a stale plugin.
RUN compose_version="$(docker compose version --short | sed 's/^v//')" \
    && printf '%s\n' 2.24.4 "$compose_version" | sort -V -C \
    || { echo "docker compose plugin $compose_version is older than 2.24.4" >&2; exit 1; }

ENV GOPROXY=off \
    GOSUMDB=off \
    GOTOOLCHAIN=local \
    CGO_ENABLED=1

# Warm the module cache from the pinned module graph, then warm the build
# cache for both flag sets the harness uses (plain `go build`/`go run` and the
# `-trimpath -buildvcs=false` adapter build). The source copy is discarded;
# only /go/pkg/mod and the GOCACHE survive into the image.
COPY go.mod go.sum /warm/
RUN GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org \
    sh -ec 'cd /warm && go mod download'
COPY . /warm/src
RUN sh -ec 'cd /warm/src \
    && go build ./... \
    && go build -trimpath -buildvcs=false ./... \
    && go vet ./... \
    && rm -rf /warm/src'

# The repo is bind-mounted and owned by an arbitrary host uid.
RUN git config --global --add safe.directory '*'

WORKDIR /work
CMD ["bash"]
