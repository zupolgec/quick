# Immagine di quick-server. Build multi-stage, binario su alpine.
# Lo stage di build gira sull'arch nativa (BUILDPLATFORM) e cross-compila verso
# TARGETOS/TARGETARCH: build buildx multi-arch veloci (niente emulazione) e, sotto
# un `docker build` normale (Coolify), le ARG sono già valorizzate da BuildKit.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-X main.version=${VERSION}" -o /quick-server ./cmd/quick-server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /quick-server /usr/local/bin/quick-server
EXPOSE 8080
ENTRYPOINT ["quick-server"]
