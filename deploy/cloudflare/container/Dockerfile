# syntax=docker/dockerfile:1
FROM node:25-alpine3.23@sha256:bdf2cca6fe3dabd014ea60163eca3f0f7015fbd5c7ee1b0e9ccb4ced6eb02ef4 AS ui
WORKDIR /src/ui
COPY ui/package*.json ./
RUN npm ci --ignore-scripts
COPY ui/ ./
RUN npm run build-enterprise

FROM golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
RUN apk add --no-cache gcc musl-dev binutils
ENV CGO_ENABLED=1 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local
WORKDIR /src
COPY core/ core/
COPY framework/ framework/
COPY plugins/ plugins/
COPY transports/ transports/
COPY integrations/headroom/*.go integrations/headroom/go.* integrations/headroom/
COPY deploy/cloudflare/container/ deploy/cloudflare/container/
COPY --from=ui /src/ui/out/ transports/bifrost-http/ui/
RUN go work init ./core ./framework ./transports ./integrations/headroom ./deploy/cloudflare/container && \
    for p in plugins/*; do if [ -f "$p/go.mod" ]; then go work use "$p"; fi; done
# Identical compiler, workspace, flags and dependency graph for executable and .so.
RUN go build -trimpath -o /out/bifrost ./transports/bifrost-http && \
    go build -trimpath -buildmode=plugin -o /out/headroom.so ./integrations/headroom && \
    go build -trimpath -o /out/launch ./deploy/cloudflare/container

FROM alpine:3.23.5@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40
RUN apk add --no-cache ca-certificates libgcc tini && addgroup -g 10001 bifrost && adduser -D -u 10001 -G bifrost bifrost
COPY --from=build /out/ /app/
USER 10001:10001
WORKDIR /app
ENV HOME=/tmp
EXPOSE 8080
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=15s --timeout=2s --start-period=60s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/sbin/tini", "--", "/app/launch"]
