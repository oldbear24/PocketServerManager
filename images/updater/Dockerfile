# The updater: a static binary on scratch, a few MB, started only for one update.
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
    -o /out/pocketcloud-updater ./cmd/updater

FROM scratch
COPY --from=build /out/pocketcloud-updater /pocketcloud-updater
LABEL org.opencontainers.image.title="pocketcloud-updater" \
      org.opencontainers.image.source="https://github.com/oldbear24/PocketServerManager" \
      pocketcloud.role="updater"
ENTRYPOINT ["/pocketcloud-updater"]
