#!/bin/sh
set -eu

datadir=/tmp/dsx-composite-mariadb
rm -rf "$datadir"
mkdir -p "$datadir"
rm -f /tmp/dsx-composite-mariadb-not-ready /tmp/dsx-composite-redis-not-ready /tmp/dsx-composite-app-not-ready
touch /tmp/dsx-composite-mariadb-not-ready /tmp/dsx-composite-redis-not-ready /tmp/dsx-composite-app-not-ready
exec mariadb-install-db \
  --auth-root-authentication-method=normal \
  --basedir=/usr \
  --datadir="$datadir" \
  --skip-test-db
