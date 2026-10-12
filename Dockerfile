# Build stage and runtime stage.
#
# Dependencies are vendored (vendor/, produced by `make vendor`), so the image
# build needs no network at all. That matters on this network, where the module
# proxy is only reachable through an HTTP proxy and is intermittent.
#
# If you build with vendor/ removed, pass the proxy:
#   docker build --build-arg HTTP_PROXY=http://192.168.188.1:3128 .

# Both bases are overridable so constrained environments (or a different
# registry) can substitute them without editing this file:
#   docker build --build-arg BUILDER_BASE=... --build-arg RUNTIME_BASE=...
ARG BUILDER_BASE=golang:1.27-alpine
FROM ${BUILDER_BASE} AS build

ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG NO_PROXY
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ARG COMMIT=none

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOPROXY=${GOPROXY} \
    GOTOOLCHAIN=local

WORKDIR /src

COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY . .

# -mod=vendor is explicit even though Go infers it: it makes the hermetic
# property a property of the build command, not of the directory listing.
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -mod=vendor -trimpath \
        -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/rms ./cmd/rms \
 && /out/rms version

ARG RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot
FROM ${RUNTIME_BASE}

# 5000 primary (multi-namespace), 5001/5002 namespace-bound mirrors, 9100 metrics.
EXPOSE 5000 5001 5002 9100

# Numeric uid rather than a name: distroless ships "nonroot" as uid 65532, but a
# substituted base may not have that account.
COPY --from=build --chown=65532:65532 /out/rms /usr/local/bin/rms
USER 65532:65532
WORKDIR /

# Distroless has no shell, so the healthcheck must be an exec-form command.
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=5 \
    CMD ["/usr/local/bin/rms", "healthcheck", "--addr", "http://127.0.0.1:5000/v2/"]

ENTRYPOINT ["/usr/local/bin/rms"]
CMD ["serve", "--config", "/etc/rms/rms.yaml"]
