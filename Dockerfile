# syntax=docker/dockerfile:1.6

ARG GO_VERSION=1.24

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
# Allow Go to auto-install the required toolchain if go.mod requests a newer version
ENV GOTOOLCHAIN=auto
RUN apk add --no-cache ca-certificates tzdata git build-base
WORKDIR /src

# Enable Go module cache for faster builds
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/agent ./cmd/agent

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/agent /agent
USER nonroot:nonroot
ENTRYPOINT ["/agent"]
