# syntax=docker/dockerfile:1
# Field Guide 03's demo image: a static page that says which tag it is, for Argo CD Image Updater to find new tags of.
# The page is made in a stage that runs on the build machine, so the final stage has only COPY and CMD and a
# two-platform build needs no emulation. Every build bakes its TAG into a layer and a label, so each tag gets its own
# digest and its own creation time (the newest-build strategy reads that time).
FROM --platform=$BUILDPLATFORM docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e AS page
ARG TAG
RUN test -n "$TAG" && mkdir /www && printf 'demo %s\n' "$TAG" > /www/index.html

FROM docker.io/library/busybox:1.38.0@sha256:fd7dc98638c8e305f4dc34e979f1c0fdfdcaeb0fbf8fcff77ae834b6da3d7e6e
ARG TAG
# The repository the image is built from: the workflow passes its own (a fork's, in a reader's fork). GitHub links the
# package to that repository through this label.
ARG SOURCE=https://github.com/liskl/k8s-notes-examples
LABEL org.opencontainers.image.source="$SOURCE" \
      org.opencontainers.image.version="$TAG"
COPY --from=page /www /www
USER 65532:65532
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
