# The panel: one static binary with the embedded UI.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG AGENT_DIGEST=
ARG UPDATER_DIGEST=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w \
      -X github.com/oldbear24/PocketServerManager/internal/version.Version=${VERSION} \
      -X github.com/oldbear24/PocketServerManager/internal/version.Commit=${COMMIT} \
      -X github.com/oldbear24/PocketServerManager/internal/version.AgentDigest=${AGENT_DIGEST} \
      -X github.com/oldbear24/PocketServerManager/internal/version.UpdaterDigest=${UPDATER_DIGEST}" \
    -o /out/pocketcloud-panel ./cmd/panel

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/pocketcloud-panel /usr/local/bin/pocketcloud-panel
ENV POCKETCLOUD_DATA_DIR=/data/pb_data POCKETCLOUD_MASTER_KEY_FILE=/run/secrets/pocketcloud-master-key
VOLUME /data
EXPOSE 8090 9443
LABEL org.opencontainers.image.title="pocketcloud-panel" \
      org.opencontainers.image.source="https://github.com/oldbear24/PocketServerManager"
ENTRYPOINT ["pocketcloud-panel"]
CMD ["serve", "--http=0.0.0.0:8090"]
