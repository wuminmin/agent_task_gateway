ARG SOURCE_DATE_EPOCH=0
FROM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS build
ARG SOURCE_DATE_EPOCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -z "$(gofmt -l evaluation/cmd/rq5-online-transition cmd/snapshot-sidecar-install cmd/oa-demo internal/oademo internal/approval)" \
	&& go test ./evaluation/cmd/rq5-online-transition ./cmd/snapshot-sidecar-install ./internal/oademo ./internal/approval \
	&& CGO_ENABLED=1 go build -trimpath -o /out/rq5-online-transition ./evaluation/cmd/rq5-online-transition \
	&& CGO_ENABLED=1 go build -trimpath -o /out/snapshot-sidecar-install ./cmd/snapshot-sidecar-install \
	&& CGO_ENABLED=1 go build -trimpath -o /out/oa-demo ./cmd/oa-demo \
	&& touch --date="@${SOURCE_DATE_EPOCH}" /out /out/rq5-online-transition /out/snapshot-sidecar-install /out/oa-demo

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS runtime
COPY --from=build /out/ /usr/local/bin/
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/rq5-online-transition"]
