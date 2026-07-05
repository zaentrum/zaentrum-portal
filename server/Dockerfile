# portal-api — the launchpad registry, folded into the zaentrum-portal repo and
# built as its own image. go.mod targets go 1.24.0; GOTOOLCHAIN=local avoids a
# toolchain download.
FROM golang:1.24-alpine AS build
ENV GOTOOLCHAIN=local
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/portal-api ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/portal-api /portal-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/portal-api"]
LABEL org.opencontainers.image.source="https://github.com/zaentrum/zaentrum-portal"
LABEL org.opencontainers.image.title="portal-api"
LABEL org.opencontainers.image.description="Launchpad registry (apps/spaces/tiles) for the zaentrum portal"
