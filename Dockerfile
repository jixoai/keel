# Build stage: static binary, no cgo. The runtime image is distroless, so the
# binary must not depend on libc or shell utilities.
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/keel ./cmd/runtime \
 && mkdir -p /out/data/cas \
 && chown -R 65532:65532 /out/data

# Runtime stage: distroless static, non-root. /data/cas carries the ownership
# that Docker copies into a fresh named volume, so the non-root user can write.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=nonroot:nonroot /out/keel /keel
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
EXPOSE 13740
ENV GOMEMLIMIT=1610612736 \
    KEEL_CAS_DIR=/data/cas
# The binary probes its own /v1/health; distroless has no shell, curl, or wget.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/keel", "-healthcheck"]
ENTRYPOINT ["/keel"]
