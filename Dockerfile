## Multi-stage Dockerfile for fathom (Fathom's agent).
##
## Stage 1 builds the static Go binary. Stage 2 runs Node-22 (for the skill
## subprocess runtime) + the fathom binary. Node-22 is needed because skills
## are TypeScript and run via the embedded runner.js; if you don't install
## any skills, you can swap the runtime to distroless / scratch instead.
##
## Build:   docker build -t zdaniels/fathom:latest .
## Run:     docker run -p 8790:8790 -v fathom_data:/data \
##              zdaniels/fathom:latest start

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO disabled: modernc.org/sqlite is pure Go, so the binary statically links
# and runs on scratch / Alpine / Debian without further runtime deps.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w -X main.Version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o /out/fathom ./cmd/fathom

FROM node:22-slim AS runtime
RUN groupadd -g 65532 fathom && useradd -m -u 65532 -g 65532 -s /usr/sbin/nologin fathom
COPY --from=builder /out/fathom /usr/local/bin/fathom
USER fathom
WORKDIR /data
EXPOSE 8790
ENV FANTAZM_VAULT_PATH=/data/vault \
    FANTAZM_VAULT_KEY=/data/.master.key \
    FANTAZM_SCHEDULE_DB=/data/schedule.db \
    FANTAZM_DATA_DIR=/data
ENTRYPOINT ["fathom"]
CMD ["start"]
