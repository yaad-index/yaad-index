#!/bin/sh
# Docker entrypoint for yaad-index. Fail-fast on missing config when
# the default `serve` path is taken; pass through everything else
# unchanged so `--help` / `--version` / ad-hoc subcommands still work.

set -eu

CFG=/etc/yaad-index/config.yaml
EXAMPLE=/etc/yaad-index/config.yaml.example
VAULT=/data/vault

case "${1:-serve}" in
    serve)
        if [ ! -f "$CFG" ]; then
            cat >&2 <<EOF
yaad-index: required config file not present at $CFG

Mount your config when running the container, e.g.

  docker run \\
    -v ./yaad-index.yaml:$CFG \\
    -v ./yaad-index.db:/data/index.db \\
    -v ./vault:$VAULT \\
    -p 7433:7433 \\
    yaad-index:latest

A starter is baked at $EXAMPLE — copy it to your host, edit, and
mount over $CFG.
EOF
            exit 1
        fi
        if [ ! -d "$VAULT" ]; then
            cat >&2 <<EOF
yaad-index: required vault directory not present at $VAULT

Mount your vault when running the container, e.g.

  docker run \\
    -v ./yaad-index.yaml:$CFG \\
    -v ./yaad-index.db:/data/index.db \\
    -v ./vault:$VAULT \\
    -p 7433:7433 \\
    yaad-index:latest

The vault is the source of truth (per ADR-0008); the index won't
operate without it.
EOF
            exit 1
        fi

        # Default --bind to 0.0.0.0:7433 and --db-path to /data/index.db
        # inside the container. The binary's defaults (localhost / XDG-
        # under-$HOME) are right for `make build` / laptop dev; the
        # container needs the public bind for `docker run -p` to work
        # and a known db path so the operator's mount actually persists
        # state. Skip each injection if operator explicitly set the
        # flag (CLI or env), so overrides win.
        bind_set=0
        db_set=0
        for arg in "$@"; do
            case "$arg" in
                --bind|--bind=*)       bind_set=1 ;;
                --db-path|--db-path=*) db_set=1 ;;
            esac
        done
        if [ "$bind_set" = "0" ] && [ -z "${YAAD_INDEX_BIND:-}" ]; then
            set -- "$@" --bind 0.0.0.0:7433
        fi
        if [ "$db_set" = "0" ] && [ -z "${YAAD_INDEX_DB_PATH:-}" ]; then
            set -- "$@" --db-path /data/index.db
        fi
        ;;
esac

exec /usr/local/bin/yaad-index "$@"
