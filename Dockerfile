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

FROM alpine:3
RUN apk add --no-cache ca-certificates wget \
    && adduser -D -u 10001 bps \
    && mkdir -p /data \
    && chown -R bps:bps /data
COPY --from=build /out/bps-server /usr/local/bin/bps-server
USER 10001:10001
WORKDIR /data
EXPOSE 8790 8791
ENTRYPOINT ["/usr/local/bin/bps-server"]
