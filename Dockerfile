# Multi-stage: build 3 binary (api, migrate, seed) lalu jalankan di distroless tanpa shell.
# Migrasi & seed ikut ter-embed (db/embed.go), jadi image tidak butuh file SQL terpisah.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/seed ./cmd/seed

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
# -tags timetzdata: zona waktu Asia/Jakarta ter-embed di binary, tidak bergantung pada image.
ENV APP_ENV=prod PORT=8080 LOG_LEVEL=info
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/api"]
