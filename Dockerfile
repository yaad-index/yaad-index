# syntax=docker/dockerfile:1.6
#
# Multi-stage build for yaad-index daemon + bundled plugin binaries.
# All binaries build from this single monorepo's source — daemon and
# plugins live in one Go module.
#
# Layout in the runtime image:
#   /usr/local/bin/yaad-index                          — server binary
#   /usr/local/lib/yaad-index/plugins/yaad-wikipedia   — bundled plugin
#   /usr/local/lib/yaad-index/plugins/yaad-bgg         — bundled plugin
#   /usr/local/lib/yaad-index/plugins/yaad-gmail       — bundled plugin
#   /usr/local/lib/yaad-index/plugins/yaad-github      — bundled plugin
#   /etc/yaad-index/config.yaml.example                — starter config
#   /usr/local/bin/docker-entrypoint.sh                — fail-fast wrapper
#
# Operator mounts at runtime:
#   /etc/yaad-index/config.yaml   (required — fail-fast if missing)
#   /data/index.db                (the SQLite file, persisted)
#   /data/vault                   (the markdown vault root)

# --- build stage ------------------------------------------------------
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=container
ENV LDFLAGS="-X 'github.com/yaad-index/yaad-index/internal/buildinfo.Version=${VERSION}'"
RUN CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o /out/yaad-index      ./cmd/yaad-index \
 && CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o /out/yaad-wikipedia  ./cmd/yaad-wikipedia \
 && CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o /out/yaad-bgg        ./cmd/yaad-bgg \
 && CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o /out/yaad-gmail      ./cmd/yaad-gmail \
 && CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o /out/yaad-github     ./cmd/yaad-github

# --- runtime ----------------------------------------------------------
# YAAD_UID / YAAD_GID default to 1000:1000 — matches the conventional
# first-user uid on debian/ubuntu/most modern distros so host-owned
# bind-mounts (yaad-index.db, vault/) are writeable without chmod.
# Override at build for non-1000 hosts:
#   make docker-build YAAD_UID=1234 YAAD_GID=1234
FROM debian:bookworm-slim
ARG YAAD_UID=1000
ARG YAAD_GID=1000
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system --gid ${YAAD_GID} yaad \
 && useradd  --system --uid ${YAAD_UID} --gid ${YAAD_GID} --home /data --shell /usr/sbin/nologin yaad \
 && mkdir -p /data /etc/yaad-index /usr/local/lib/yaad-index/plugins \
 && chown -R yaad:yaad /data

COPY --from=build /out/yaad-index       /usr/local/bin/yaad-index
COPY --from=build /out/yaad-wikipedia   /usr/local/lib/yaad-index/plugins/yaad-wikipedia
COPY --from=build /out/yaad-bgg         /usr/local/lib/yaad-index/plugins/yaad-bgg
COPY --from=build /out/yaad-gmail       /usr/local/lib/yaad-index/plugins/yaad-gmail
COPY --from=build /out/yaad-github      /usr/local/lib/yaad-index/plugins/yaad-github
COPY config.yaml.example                /etc/yaad-index/config.yaml.example
COPY docker-entrypoint.sh               /usr/local/bin/docker-entrypoint.sh

RUN chmod +x /usr/local/bin/yaad-index \
             /usr/local/lib/yaad-index/plugins/yaad-wikipedia \
             /usr/local/lib/yaad-index/plugins/yaad-bgg \
             /usr/local/lib/yaad-index/plugins/yaad-gmail \
             /usr/local/lib/yaad-index/plugins/yaad-github \
             /usr/local/bin/docker-entrypoint.sh

USER yaad
EXPOSE 7433
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["serve", "--config", "/etc/yaad-index/config.yaml"]
