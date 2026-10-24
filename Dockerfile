# Build on the native platform and cross-compile, so multi-arch images don't need QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /cronbeat .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/ekiprop/cronbeat" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
COPY --from=build /cronbeat /cronbeat
ENV CRONBEAT_DB=/data/cronbeat.db CRONBEAT_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/cronbeat", "serve"]
