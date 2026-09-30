# linux/amd64 platform manifests, resolved from official version tags on 2026-09-23.
FROM tailscale/tailscale@sha256:9482efa70e5e37180a74ad17e01fb0a43ec4b629cdde1a28a9fb79597bca63e2 AS tailscale
FROM python@sha256:9e28339897646e00f176fbc1694e444b56d98a332bda2d8d080da96dd45112eb
RUN apk add --no-cache tini=0.19.0-r3
COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscaled /usr/local/bin/containerboot /usr/local/bin/
COPY container/ /app/
ENV TS_USERSPACE=true TS_STATE_DIR="" TS_KUBE_SECRET="" \
    TS_AUTH_ONCE=false TS_HOSTNAME=cf-tailnet-proof \
    TS_EXTRA_ARGS="--advertise-tags=tag:cf-proof --accept-dns=false" \
    TS_SOCKET=/tmp/tailscaled.sock TS_SERVE_CONFIG=/app/serve.json \
    TS_ENABLE_HEALTH_CHECK=true TS_LOCAL_ADDR_PORT=127.0.0.1:9002 \
    TS_ENABLE_METRICS=false TS_EXPERIMENTAL_SERVICE_AUTO_ADVERTISEMENT=false \
    PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
USER 65532:65532
HEALTHCHECK --interval=15s --timeout=3s --start-period=90s --retries=3 \
    CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:9002/healthz', timeout=2); urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2)"
ENTRYPOINT ["/sbin/tini", "--", "python", "/app/supervise.py"]
