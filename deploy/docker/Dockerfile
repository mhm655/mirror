# MIRROR — one image, engine + API + UI.
#
# Multi-stage: build the UI, build the Go binary against the built UI's
# output directory, then ship both in a minimal runtime image. See
# docs/BLUEPRINT.md section 18 for why this is one process rather than a
# server image plus a separate static-file server.

FROM node:20-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download 2>/dev/null || true
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
      -o /out/mirrord ./cmd/mirrord
RUN CGO_ENABLED=0 go build -trimpath -o /out/mirrorbench ./cmd/mirrorbench

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/mirrord /app/mirrord
COPY --from=build /out/mirrorbench /app/mirrorbench
COPY --from=web /src/web/dist /app/web/dist

ENV MIRROR_ADDR=:8080 \
    MIRROR_WEB_DIR=/app/web/dist \
    MIRROR_DATA_DIR=/data \
    MIRROR_AUTH_MODE=production

VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/mirrord"]
