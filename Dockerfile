# Build the static router. CGO is off, so the result runs on any glibc/musl host
# and needs nothing from the runtime image.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/cottenrouter ./cmd/cottenrouter

# distroless/static:nonroot has no shell, no package manager, and runs as UID
# 65532. The router listens on unprivileged 5353 inside the container, so it
# needs neither root nor NET_BIND_SERVICE; publish host 53 to 5353 instead.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/cottenrouter /usr/local/bin/cottenrouter
COPY cottenrouter.docker.json /etc/cottenrouter/config.json

USER nonroot:nonroot
EXPOSE 5353/udp 5353/tcp 8853/tcp 8443/tcp

# The binary is its own health probe: the image carries no curl or wget.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/cottenrouter", "healthz", "-config", "/etc/cottenrouter/config.json"]

ENTRYPOINT ["/usr/local/bin/cottenrouter"]
CMD ["serve", "-config", "/etc/cottenrouter/config.json"]
