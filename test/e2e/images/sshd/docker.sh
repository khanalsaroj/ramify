#!/bin/sh
# A minimal fake `docker` CLI, understanding only the exact subset of
# `docker compose` invocations providers/deploy/compose issues:
#   IMAGE_TAG=x COMPOSE_PROJECT_NAME=y docker compose -f <file> up -d
#   docker compose -p <name> -f <file> {stop|start|down --volumes --remove-orphans}
#   docker compose -p <name> -f <file> ps --status running -q
#   docker compose -p <name> -f <file> logs --no-color --tail=500
# It exists so the e2e harness exercises the real SSH transport
# (golang.org/x/crypto/ssh dial + auth + exec) without needing Docker-in-Docker.
set -eu

STATE_DIR=/var/lib/fakedocker
mkdir -p "$STATE_DIR"

if [ "${1:-}" != "compose" ]; then
    echo "fake docker: unsupported command: $*" >&2
    exit 1
fi
shift

PROJECT="${COMPOSE_PROJECT_NAME:-}"
ACTION=""
while [ $# -gt 0 ]; do
    case "$1" in
        -p) PROJECT="$2"; shift 2 ;;
        up|down|stop|start|ps|logs) ACTION="$1"; shift ;;
        *) shift ;;
    esac
done

if [ -z "$PROJECT" ]; then
    echo "fake docker: no project name (missing -p or COMPOSE_PROJECT_NAME)" >&2
    exit 1
fi

STATE_FILE="$STATE_DIR/$PROJECT"

case "$ACTION" in
    up)
        {
            echo "running"
            echo "image_tag=${IMAGE_TAG:-unknown}"
        } > "$STATE_FILE"
        echo "fake docker: up $PROJECT (image=${IMAGE_TAG:-unknown})"
        ;;
    down)
        rm -f "$STATE_FILE"
        echo "fake docker: down $PROJECT"
        ;;
    stop)
        if [ -f "$STATE_FILE" ]; then
            sed -i '1s/.*/stopped/' "$STATE_FILE"
        fi
        echo "fake docker: stop $PROJECT"
        ;;
    start)
        if [ -f "$STATE_FILE" ]; then
            sed -i '1s/.*/running/' "$STATE_FILE"
        fi
        echo "fake docker: start $PROJECT"
        ;;
    ps)
        if [ -f "$STATE_FILE" ] && [ "$(head -n1 "$STATE_FILE")" = "running" ]; then
            echo "fake-container-id-$PROJECT"
        fi
        ;;
    logs)
        if [ -f "$STATE_FILE" ]; then
            echo "fake logs for $PROJECT:"
            cat "$STATE_FILE"
        else
            echo "fake docker: no such project: $PROJECT" >&2
            exit 1
        fi
        ;;
    *)
        echo "fake docker: unsupported compose action: $ACTION" >&2
        exit 1
        ;;
esac
