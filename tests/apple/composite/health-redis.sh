#!/bin/sh
set -eu

[ "$(redis-cli -h 127.0.0.1 -p 6379 ping)" = "PONG" ]
rm -f /tmp/dsx-composite-redis-not-ready
