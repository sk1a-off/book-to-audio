# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" -o /out/audiobook-api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM scratch AS runtime

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/audiobook-api /usr/local/bin/audiobook-api
COPY --from=build /out/migrate /usr/local/bin/migrate

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/audiobook-api"]
