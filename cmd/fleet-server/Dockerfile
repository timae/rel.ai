# syntax=docker/dockerfile:1.6

# Multi-stage build: compile the fleet-server binary and ship it on top of a
# minimal distroless base. The binary is pure Go (modernc sqlite, no CGO), so
# we get a fully static image.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache modules separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/fleet-server \
      ./cmd/fleet-server

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/fleet-server /app/fleet-server

# The coordinator's SQLite database lives here — mount a volume at /data.
ENV FLEET_DB=/data/fleet.db
VOLUME ["/data"]

EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/app/fleet-server"]
