# syntax=docker/dockerfile:1
# Field Guide 07's demo image: one static Go binary, /cnpg-demo, on distroless static. The build stage
# runs on the build machine and cross-compiles, so the final stage holds only COPY and a two-platform build
# needs no emulation.
ARG GO_DIGEST=sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b
ARG STATIC_DIGEST=sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.3@$GO_DIGEST AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS TARGETARCH TAG=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$TAG" -o /out/cnpg-demo .

FROM gcr.io/distroless/static-debian13:nonroot@$STATIC_DIGEST
ARG TAG=dev
ARG SOURCE=https://github.com/liskl/k8s-notes-examples
LABEL org.opencontainers.image.source="$SOURCE" \
      org.opencontainers.image.version="$TAG"
COPY --from=build /out/cnpg-demo /cnpg-demo
USER 65532:65532
ENTRYPOINT ["/cnpg-demo"]
