#!/bin/sh
set -eu
cd "$(dirname "$0")"
umask 077
mkdir -p secrets
chmod 700 secrets
[ ! -d data/mysql ] || { echo 'Database already initialized in ./data; change its password using ALTER USER, not this setup script.' >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo 'Docker access is required to check for an initialized database. Run with sudo.' >&2; exit 1; }
if docker volume inspect fsd_mysql_data >/dev/null 2>&1; then
    volume_state=$(docker run --rm --network none -v fsd_mysql_data:/var/lib/mysql:ro --entrypoint sh mysql:8.4 -ec 'if [ -d /var/lib/mysql/mysql ]; then echo initialized; else echo empty; fi')
    if [ "$volume_state" = initialized ]; then
        echo 'Database already initialized in fsd_mysql_data; use ALTER USER and update the password file together.' >&2
        exit 1
    fi
fi
[ -t 0 ] || { echo 'Run this script interactively in your NAS terminal.' >&2; exit 1; }
terminal_state=$(stty -g)
trap 'stty "$terminal_state"; unset first second' EXIT
trap 'exit 1' HUP INT TERM
printf 'Enter your new MySQL root password (at least 16 characters): ' >&2
stty -echo
IFS= read -r first
printf '\nConfirm root password: ' >&2
IFS= read -r second
stty "$terminal_state"
printf '\n' >&2
[ "$first" = "$second" ] || { echo 'Passwords do not match; nothing changed.' >&2; exit 1; }
[ "${#first}" -ge 16 ] || { echo 'Password is too short; nothing changed.' >&2; exit 1; }
if [ -f secrets/mysql_root_password.txt ]; then
    cp secrets/mysql_root_password.txt "secrets/mysql_root_password.before-$(date +%Y%m%d%H%M%S).txt"
fi
printf '%s\n' "$first" > secrets/mysql_root_password.txt
chmod 600 secrets/mysql_root_password.txt
unset first second
echo 'Root password saved for initialization; password not printed.'
