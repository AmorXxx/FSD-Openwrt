#!/bin/sh
set -eu
# Docker Compose file-backed secrets may be root-readable only. Copy TLS
# material to an internal directory readable only by the MySQL service user.
mkdir -p /run/fsd-mysql-tls
chmod 700 /run/fsd-mysql-tls
cp /run/secrets/mysql_tls_ca /run/fsd-mysql-tls/ca.crt
cp /run/secrets/mysql_tls_cert /run/fsd-mysql-tls/server.crt
cp /run/secrets/mysql_tls_key /run/fsd-mysql-tls/server.key
chmod 600 /run/fsd-mysql-tls/*
chown -R mysql:mysql /run/fsd-mysql-tls
exec /usr/local/bin/docker-entrypoint.sh "$@"
