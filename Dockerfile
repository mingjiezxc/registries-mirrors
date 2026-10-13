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
#
# Both are declared *before* the first FROM on purpose: an ARG declared inside a
# stage is out of scope for a later FROM, and current BuildKit refuses to build
# the second stage at all in that case
# ("base name (${RUNTIME_BASE}) should not be blank").
ARG BUILDER_BASE=golang:1.27-alpine
ARG RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot

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

FROM ${RUNTIME_BASE}

# 5000 primary (multi-namespace, the only content endpoint), 9100 metrics.
# 5001/5002 曾用于「按 namespace 绑定的镜像端口」，该特性已删除（见 deploy/103/README.md），
# 因此不再 EXPOSE——留着会让 `docker ps` 显示一组并不存在的端口。
EXPOSE 5000 9100

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
