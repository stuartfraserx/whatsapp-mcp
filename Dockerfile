FROM golang:1.26.0-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go; no C toolchain is needed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/whatsapp-mcp ./cmd/whatsapp-mcp

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 whatsapp && useradd --uid 10001 --gid 10001 --no-create-home whatsapp \
    && mkdir /data && chown 10001:10001 /data
COPY --from=build /out/whatsapp-mcp /usr/local/bin/whatsapp-mcp
ENV WHATSAPP_MCP_DATA_DIR=/data WHATSAPP_MCP_LOG_QR=1
VOLUME /data
USER 10001:10001
# exec preserves signal handling; the shell expands Railway's PORT.
CMD ["/bin/sh", "-c", "exec whatsapp-mcp serve --transport http --http-addr 0.0.0.0:${PORT:-8080}"]
