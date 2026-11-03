# The agent container: the static agent binary, the Docker CLI with the Compose
# plugin (from the official image) and restic.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/oldbear24/PocketServerManager/internal/version.Version=${VERSION} -X github.com/oldbear24/PocketServerManager/internal/version.Commit=${COMMIT}" \
    -o /out/pocketcloud-agent ./cmd/agent

FROM docker:27-cli
RUN apk add --no-cache restic ca-certificates tzdata
COPY --from=build /out/pocketcloud-agent /usr/local/bin/pocketcloud-agent
VOLUME /var/lib/pocketcloud
LABEL org.opencontainers.image.title="pocketcloud-agent" \
      org.opencontainers.image.source="https://github.com/oldbear24/PocketServerManager" \
      pocketcloud.role="agent"
# Healthy only while connected to the panel; the updater relies on this.
HEALTHCHECK --interval=5s --timeout=3s --start-period=20s --retries=3 \
    CMD ["pocketcloud-agent", "healthcheck"]
ENTRYPOINT ["pocketcloud-agent"]
CMD ["run"]
