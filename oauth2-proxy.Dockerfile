# oauth2-proxy ufficiale (immagine distroless: niente shell/wget/curl) + il nostro
# mini binario statico `healthcheck`, solo per il check HTTP di /ping. È il modo
# idiomatico per gli HEALTHCHECK su distroless: un singolo eseguibile, non busybox.
# Senza, Coolify mostra "running (unknown)".
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/healthcheck ./cmd/healthcheck
RUN CGO_ENABLED=0 go build -o /healthcheck ./cmd/healthcheck

FROM quay.io/oauth2-proxy/oauth2-proxy:latest
COPY --from=build /healthcheck /healthcheck
HEALTHCHECK --interval=15s --timeout=4s --start-period=5s --retries=5 \
    CMD ["/healthcheck", "http://127.0.0.1:4180/ping"]
