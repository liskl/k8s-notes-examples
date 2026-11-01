# syntax=docker/dockerfile:1
# Field Guide 06's demo image: one static Go binary, /keda-demo, on distroless static. The build stage
# runs on the build machine and cross-compiles, so the final stage holds only COPY and a two-platform build
# needs no emulation.
ARG GO_DIGEST=sha256:670bb5aa5349ae03b8516764718486f4f16399134bdc9acd0c46a23f043ab05c
ARG STATIC_DIGEST=sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.10@$GO_DIGEST AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY externalscaler/*.go externalscaler/
ARG TARGETOS TARGETARCH TAG=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$TAG" -o /out/keda-demo .

FROM gcr.io/distroless/static-debian13:nonroot@$STATIC_DIGEST
ARG TAG=dev
ARG SOURCE=https://github.com/liskl/k8s-notes-examples
LABEL org.opencontainers.image.source="$SOURCE" \
      org.opencontainers.image.version="$TAG"
COPY --from=build /out/keda-demo /keda-demo
USER 65532:65532
ENTRYPOINT ["/keda-demo"]
