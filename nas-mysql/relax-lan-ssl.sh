#!/bin/sh
set -eu
# Run on NAS after MySQL has finished initialization. Password never leaves the container.
docker exec -i fsd-mysql sh -c 'export MYSQL_PWD="$(cat /run/secrets/mysql_root_password)"; exec mysql --protocol=socket -uroot' <<'SQL'
ALTER USER 'root'@'192.168.1.0/255.255.255.0' REQUIRE NONE;
SET GLOBAL require_secure_transport = OFF;
SELECT user, host, ssl_type FROM mysql.user WHERE user IN ('root', 'fsd_ingest');
SQL
echo 'LAN root SSL requirement removed. Keep the updated Compose file so this also survives restarts.'
