#!/bin/sh
set -eu

mariadb-admin --protocol=tcp --host=127.0.0.1 --port=3306 --user=root ping >/dev/null
rm -f /tmp/dsx-composite-mariadb-not-ready
