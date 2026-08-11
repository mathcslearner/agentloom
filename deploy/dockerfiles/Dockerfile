# agentloom binaries, one image per deployable via build targets:
#
#   docker build -f deploy/dockerfiles/Dockerfile --target api     .
#   docker build -f deploy/dockerfiles/Dockerfile --target worker  .
#   docker build -f deploy/dockerfiles/Dockerfile --target migrate .
#
# Ticket 4.6: a simple builder stage is deliberate — hardening (distroless,
# SBOM, provenance) is M20's. The compose file selects targets for the
# `app` profile services.

FROM golang:1.26-alpine AS build
WORKDIR /src
# Version stamped into internal/version via ldflags; "dev" outside CI.
ARG VERSION=dev
# Dependency layer first so code edits don't re-download modules.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
      -ldflags "-X github.com/mathcslearner/agentloom/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/api ./cmd/worker ./cmd/migrate

# Shared runtime base: non-root, busybox wget available for healthchecks.
FROM alpine:3.22 AS base
RUN adduser -D -u 10001 agentloom
USER agentloom

FROM base AS api
COPY --from=build /out/api /usr/local/bin/agentloom-api
EXPOSE 8080
ENTRYPOINT ["agentloom-api"]

FROM base AS worker
COPY --from=build /out/worker /usr/local/bin/agentloom-worker
ENTRYPOINT ["agentloom-worker"]

FROM base AS migrate
COPY --from=build /out/migrate /usr/local/bin/agentloom-migrate
ENTRYPOINT ["agentloom-migrate"]
CMD ["up"]
