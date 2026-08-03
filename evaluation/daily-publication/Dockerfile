ARG SOURCE_DATE_EPOCH=0
FROM golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 AS build
ARG SOURCE_DATE_EPOCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -z "$(gofmt -l evaluation/daily-publication evaluation/cmd/v4-offline)" \
    && go test ./evaluation/daily-publication/cmd/phase ./evaluation/cmd/v4-offline \
    && CGO_ENABLED=1 go build -trimpath -o /out/v4-offline ./evaluation/cmd/v4-offline \
    && CGO_ENABLED=1 go build -trimpath -o /out/daily-publication-phase ./evaluation/daily-publication/cmd/phase \
    && touch --date="@${SOURCE_DATE_EPOCH}" /out /out/v4-offline /out/daily-publication-phase

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS phase
COPY --from=build /out/ /usr/local/bin/
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/daily-publication-phase"]
