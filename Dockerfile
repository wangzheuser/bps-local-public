# Build the managed multi-account bridge into a minimal container.
FROM golang:1.26-alpine AS build
# GOPROXY lets restricted networks point the build at a reachable module
# mirror, e.g. --build-arg GOPROXY=https://goproxy.cn,direct. Empty uses the
# Go default (https://proxy.golang.org,direct).
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w" \
    -o /out/bps-server ./cmd/bps-server

# Fetch the ModelTrace fingerprint bank so model attribution works out of the
# box. This is optional: a failed download leaves attribution disabled (the
# console says so) but does not break the build or the server.
ARG MODELTRACE_BANK_URL=https://raw.githubusercontent.com/xqy2006/ModelTrace/main/data/unified_bank.json
RUN mkdir -p /out/bank \
    && (wget -q -T 30 -O /out/bank/unified_bank.json "$MODELTRACE_BANK_URL" \
        && head -c 1 /out/bank/unified_bank.json | grep -q '{' \
        && echo "fingerprint bank downloaded: $(wc -c < /out/bank/unified_bank.json) bytes") \
       || (rm -f /out/bank/unified_bank.json \
        && echo "WARNING: fingerprint bank unavailable; model attribution will be disabled")
# A placeholder keeps the COPY below valid when the download failed; the loader
# ignores non-bank files it cannot parse and reports attribution as disabled.
RUN [ -f /out/bank/unified_bank.json ] || touch /out/bank/.absent

FROM alpine:3
RUN apk add --no-cache ca-certificates wget \
    && adduser -D -u 10001 bps \
    && mkdir -p /data /usr/local/share/modeltrace \
    && chown -R bps:bps /data
COPY --from=build /out/bps-server /usr/local/bin/bps-server
# The loader probes this path automatically, so no env var is required.
COPY --from=build /out/bank/ /usr/local/share/modeltrace/
USER 10001:10001
WORKDIR /data
EXPOSE 8790 8791
ENTRYPOINT ["/usr/local/bin/bps-server"]
