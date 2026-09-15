FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/fathom ./cmd/fathom

FROM node:22-slim AS runtime
RUN groupadd -g 65532 fathom && useradd -u 65532 -g 65532 -d /data/home -s /usr/sbin/nologin fathom \
    && mkdir -p /data /etc/fathom && chown 65532:65532 /data
COPY --from=builder /out/fathom /usr/local/bin/fathom
COPY deploy/container.config.yaml /etc/fathom/config.yaml
COPY deploy/container.policy.yaml /etc/fathom/policy.yaml
COPY --chmod=755 deploy/entrypoint.sh /usr/local/bin/fathom-entrypoint
ENV HOME=/data/home \
    XDG_CACHE_HOME=/data/cache \
    FATHOM_CONFIG=/etc/fathom/config.yaml \
    FATHOM_VAULT_PATH=/data/vault \
    FATHOM_VAULT_KEY=/data/.master.key \
    FATHOM_DEVICES_DB=/data/devices.db \
    FATHOM_TOKEN_FILE=/data/api-token \
    FATHOM_THREADS_DB=/data/threads.db \
    FATHOM_SCHEDULE_DB=/data/schedule.db \
    FATHOM_DATA_DIR=/data
USER 65532:65532
WORKDIR /data
EXPOSE 8790
ENTRYPOINT ["fathom-entrypoint"]
CMD ["start"]
