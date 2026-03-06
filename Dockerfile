# Build stage
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Copy everything (go.mod, go.sum, and source).
COPY . .

# Resolve dependencies and build.
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w" \
    -a -o manager ./cmd/main.go

# Runtime stage — use distroless for minimal attack surface.
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
