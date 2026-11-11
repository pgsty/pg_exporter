# syntax=docker/dockerfile:1
FROM golang:1.27.4-alpine AS builder-env

RUN apk add --no-cache ca-certificates

ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}

# Build a self-contained pg_exporter container with a clean environment and no
# dependencies.
#
# build with
#
#   docker buildx build -f Dockerfile --tag pg_exporter .
#

WORKDIR /build

COPY go.mod go.sum ./
RUN \
  --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 GOOS=linux go mod download

COPY . /build
ARG VERSION=v1.6.1
ARG BRANCH=main
ARG REVISION=HEAD
ARG BUILD_DATE=unknown
RUN \
  --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 GOOS=linux go build -a \
    -ldflags="-s -w -extldflags '-static' -X 'pg_exporter/exporter.Version=${VERSION}' -X 'pg_exporter/exporter.Branch=${BRANCH}' -X 'pg_exporter/exporter.Revision=${REVISION}' -X 'pg_exporter/exporter.BuildDate=${BUILD_DATE}'" \
    -o /pg_exporter .

FROM scratch
LABEL org.opencontainers.image.authors="Ruohang Feng <rh@vonng.com>" \
      org.opencontainers.image.url="https://github.com/pgsty/pg_exporter" \
      org.opencontainers.image.source="https://github.com/pgsty/pg_exporter" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="pg_exporter" \
      org.opencontainers.image.description="PostgreSQL/Pgbouncer metrics exporter for Prometheus"

WORKDIR /bin
COPY --from=builder-env /pg_exporter /bin/pg_exporter
COPY --from=builder-env /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY pg_exporter.yml /etc/pg_exporter.yml
COPY LICENSE /LICENSE
EXPOSE 9630/tcp
ENTRYPOINT ["/bin/pg_exporter"]
