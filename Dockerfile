# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/internet-monitor ./cmd/internet-monitor

# distroless/static includes CA certificates and tzdata and runs as root by
# default. The app needs root only to open its raw ICMP socket, then drops to
# PUID:PGID (99:100 by default, Unraid's nobody:users).
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/internet-monitor /internet-monitor
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 8765
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s \
    CMD ["/internet-monitor", "healthcheck"]
ENTRYPOINT ["/internet-monitor"]
CMD ["serve"]
