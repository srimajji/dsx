#!/bin/sh
set -eu

if [ -e /tmp/dsx-composite-app-not-ready ]; then
  printf 'Caddy launched before application readiness\n' >&2
  exit 1
fi
body="$(wget -qO- http://127.0.0.1:3000/health)"
if [ "$body" != "mariadb=ready redis=ready" ]; then
  printf 'application was not ready before Caddy launch\n' >&2
  exit 1
fi
exec /usr/sbin/caddy run --config /opt/dsx-composite/Caddyfile --adapter caddyfile
