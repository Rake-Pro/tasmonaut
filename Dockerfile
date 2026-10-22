# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27.3 AS build
WORKDIR /src

# Dependencies are vendored, so the build needs no network.
COPY . .
# VERSION is injected by CI (the release tag) and stamped into the binary.
ARG VERSION=dev
# Pure Go, CGO off: the binary is static and runs on distroless/static.
RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/tasmonaut ./cmd/tasmonaut

# ---- runtime ----
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/tasmonaut /usr/local/bin/tasmonaut
# The only outbound TLS request is the optional GitHub "latest release" check,
# and it needs a CA bundle. Pin it rather than rely on the base image.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# tasmonaut keeps no state: there is no volume. Configure it with TASMONAUT_*
# environment variables or mount a YAML file and set TASMONAUT_CONFIG.
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/tasmonaut"]
