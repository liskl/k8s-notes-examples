# syntax=docker/dockerfile:1
# Field Guide 09's DNS-01 webhook solver: one static Go binary, /lab-dns-webhook, on distroless static. The
# build stage runs on the build machine and cross-compiles, so the final stage holds only COPY and a
# two-platform build needs no emulation. The vet and test step comes before the build, so an image cannot
# be made from code whose tests fail.
ARG GO_DIGEST=sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d
ARG STATIC_DIGEST=sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.4@$GO_DIGEST AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go vet ./... && go test ./...
ARG TARGETOS TARGETARCH TAG=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$TAG" -o /out/lab-dns-webhook .

FROM gcr.io/distroless/static-debian13:nonroot@$STATIC_DIGEST
ARG TAG=dev
ARG SOURCE=https://github.com/liskl/k8s-notes-examples
LABEL org.opencontainers.image.source="$SOURCE" \
      org.opencontainers.image.version="$TAG"
COPY --from=build /out/lab-dns-webhook /lab-dns-webhook
USER 65532:65532
ENTRYPOINT ["/lab-dns-webhook"]
