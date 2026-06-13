# Multi-stage build. modernc.org/sqlite is pure Go, so CGO can stay off and
# the result is a static binary that runs on distroless/static.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /api ./cmd/api

# Runtime: distroless static (runs as root, so /app stays writable for the
# SQLite WAL files). Cloud Run overlays an in-memory writable layer, so the
# baked DB is read instantly and any cloud-side scrape writes there — but
# that overlay is ephemeral, resetting to the baked seed on each cold start.
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /api /app/api
COPY cuanto_cuesta.db /app/cuanto_cuesta.db
EXPOSE 8080
ENTRYPOINT ["/app/api", "-db", "/app/cuanto_cuesta.db"]
