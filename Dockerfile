FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /cronbeat .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /cronbeat /cronbeat
ENV CRONBEAT_DB=/data/cronbeat.db CRONBEAT_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/cronbeat", "serve"]
