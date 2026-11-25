# Multi-stage build: compile a static binary, then copy it into a minimal
# runtime image. No package manager, no shell, no root user.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/starlink-manager ./cmd/starlink-manager
# Prepare the persistence directory with the ownership of the nonroot user
# (uid/gid 65532 in distroless). A named volume mounted on /data inherits
# this ownership on first use, so writes do not fail with EACCES.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/starlink-manager /usr/bin/starlink-manager
COPY --from=build --chown=65532:65532 /out/data /data
# Mount a volume on /data for persistent history (compose/Salt stack).
ENV LISTEN_ADDR=":8080" \
    STARLINK_DATA_DIR="/data" \
    AUTH_MODE="oidc"
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/starlink-manager"]
