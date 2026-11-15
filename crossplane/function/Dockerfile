# syntax=docker/dockerfile:1
# Field Guide 08's composition function, function-appdb-schemas: the runtime image. One static Go binary,
# /function, on distroless static. The build stage runs on the build machine and cross-compiles, so a
# two-platform build needs no emulation. This image is only half of what Crossplane installs: the
# workflow wraps it in a package (an xpkg) with package/crossplane.yaml.
ARG GO_DIGEST=sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d
ARG STATIC_DIGEST=sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.4@$GO_DIGEST AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /fn
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY input/ input/
ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /function .

FROM gcr.io/distroless/static-debian13:nonroot@$STATIC_DIGEST
ARG SOURCE=https://github.com/liskl/k8s-notes-examples
LABEL org.opencontainers.image.source="$SOURCE"
COPY --from=build /function /function
EXPOSE 9443
USER 65532:65532
ENTRYPOINT ["/function"]
