#!/bin/sh
set -eu
cd "$(dirname "$0")"
host=${1:-}
lan_ip=${2:-}
case "$host" in ''|*[!A-Za-z0-9.-]*) echo 'Usage: sh prepare.sh <DDNS-hostname> <NAS-LAN-IPv4>' >&2; exit 1;; esac
printf '%s\n' "$lan_ip" | awk -F. 'NF!=4 {exit 1} {for(i=1;i<=4;i++) if($i !~ /^[0-9]+$/ || $i>255) exit 1}' || { echo 'Invalid NAS IPv4 address' >&2; exit 1; }
[ ! -d data/mysql ] || { echo 'An initialized database exists in ./data. Stop and migrate it before switching to the managed volume.' >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo 'openssl is required.' >&2; exit 1; }
umask 077
mkdir -p secrets/tls client
chmod 700 secrets secrets/tls
if [ ! -s secrets/mysql_root_password.txt ]; then
    sh ./set-root-password.sh
fi
printf 'MYSQL_LAN_IP=%s\n' "$lan_ip" > .env
for name in mysql_ingest_password upload_token; do
    file="secrets/$name.txt"
    if [ -e "$file" ]; then
        [ -s "$file" ] || { echo "Empty password file: $file" >&2; exit 1; }
    else
        openssl rand -hex 32 > "$file"
    fi
    chmod 600 "$file"
done
if [ -e secrets/tls/server.crt ]; then
    if ! openssl x509 -in secrets/tls/server.crt -noout -checkip "$lan_ip" | grep -q 'does match certificate'; then
        echo 'Updating internal DB certificate SAN to include the NAS LAN IP; keeping CA and private key.'
        cp secrets/tls/server.crt "secrets/tls/server.crt.before-lan-$(date +%Y%m%d%H%M%S)"
        openssl req -new -key secrets/tls/server.key -subj "/CN=$host" -out secrets/tls/server.csr
        printf 'subjectAltName=DNS:%s,DNS:localhost,IP:127.0.0.1,IP:%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$host" "$lan_ip" > secrets/tls/server.ext
        openssl x509 -req -in secrets/tls/server.csr -CA secrets/tls/ca.crt -CAkey secrets/tls/ca.key -CAcreateserial -out secrets/tls/server.crt.new -days 825 -sha256 -extfile secrets/tls/server.ext
        mv secrets/tls/server.crt.new secrets/tls/server.crt
        rm -f secrets/tls/server.csr secrets/tls/server.ext
    fi
    openssl x509 -in secrets/tls/server.crt -noout -checkhost "$host" | grep -q 'does match certificate' || { echo 'Existing TLS certificate does not match hostname.' >&2; exit 1; }
    openssl verify -CAfile secrets/tls/ca.crt secrets/tls/server.crt >/dev/null
    openssl x509 -in secrets/tls/server.crt -noout -checkip 127.0.0.1 | grep -q 'does match certificate' || { echo 'Existing certificate lacks 127.0.0.1 SAN; renew the server certificate before starting host networking.' >&2; exit 1; }
    [ -s secrets/tls/server.key ] || { echo 'Missing server private key' >&2; exit 1; }
else
    # Never overwrite an incomplete or existing private CA silently.
    [ ! -e secrets/tls/ca.key ] && [ ! -e secrets/tls/server.key ] || { echo 'Partial TLS files exist; inspect them first.' >&2; exit 1; }
    openssl req -x509 -newkey rsa:3072 -nodes -sha256 -days 3650 \
      -subj '/CN=FSD MySQL private CA' -keyout secrets/tls/ca.key -out secrets/tls/ca.crt 2>/dev/null
    openssl req -newkey rsa:3072 -nodes -sha256 -subj "/CN=$host" \
      -keyout secrets/tls/server.key -out secrets/tls/server.csr 2>/dev/null
    printf 'subjectAltName=DNS:%s,DNS:localhost,IP:127.0.0.1,IP:%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$host" "$lan_ip" > secrets/tls/server.ext
    openssl x509 -req -in secrets/tls/server.csr -CA secrets/tls/ca.crt -CAkey secrets/tls/ca.key \
      -CAcreateserial -out secrets/tls/server.crt -days 825 -sha256 -extfile secrets/tls/server.ext 2>/dev/null
    rm -f secrets/tls/server.csr secrets/tls/server.ext
fi
chmod 600 secrets/tls/*
cp secrets/tls/ca.crt client/mysql-ca.crt
chmod 644 client/mysql-ca.crt
cp secrets/upload_token.txt client/upload-token
chmod 600 client/upload-token
printf '{\n  "enabled": false,\n  "local_csv": false,\n  "endpoint": "https://%s:5001/v1/events",\n  "device_id": "mu300",\n  "token_file": "/etc/fsd/upload-token"\n}\n' "$host" > client/event-upload.json
chmod 600 client/event-upload.json
printf 'Ready: MySQL 127.0.0.1 and %s port 13306; receiver 127.0.0.1:18080.\n' "$lan_ip"
echo 'Device files: client/event-upload.json and client/upload-token. Keep them private.'
echo 'Next: docker compose up -d --build'
