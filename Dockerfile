FROM golang:1.10.1 AS build

ARG ARCH=amd64
ARG VERSION=wpg
ARG GIT_COMMIT=dirty
ARG PKG=k8s.io/ingress-gce

ENV ARCH=${ARCH} PKG=${PKG} VERSION=${VERSION} GIT_COMMIT=${GIT_COMMIT} TARGET=glbc GOPATH=/go
COPY . /go/src/${PKG}
WORKDIR /go/src/${PKG}

RUN ["/bin/bash", "build/build.sh"]

FROM alpine:3.8

ARG ARCH=amd64

RUN apk add --no-cache ca-certificates

COPY --from=build /go/bin/linux_${ARCH}/glbc /glbc
ENTRYPOINT ["/glbc"]
