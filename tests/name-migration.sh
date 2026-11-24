#!/bin/sh
# Disposable Linux container only. /work is this project's source directory.
set -eu
mkdir -p /etc/init.d /etc/config /usr/share/fsd /lib/apk/packages /etc/legacy-fsd/profiles
printf '1.6.1-r1\n' > /usr/share/fsd/package-version
printf '/etc/init.d/legacy-fsd\n/usr/sbin/legacy-fsd\n' > /lib/apk/packages/luci-app-fsd.list
cat > /etc/init.d/legacy-fsd <<'SERVICE'
#!/bin/sh
# read -r lan_prefix < /etc/legacy-fsd/lan-prefix
printf '%s\n' "$1" >> /tmp/legacy-service-calls
SERVICE
chmod 755 /etc/init.d/legacy-fsd
printf 'private-business-config\n' > /etc/legacy-fsd/profiles/default.yaml
ln -s profiles/default.yaml /etc/legacy-fsd/config.yaml
printf '192.168.50.1/24\n' > /etc/legacy-fsd/lan-prefix
printf 'private-certificate\n' > /etc/legacy-fsd/local.crt
printf 'reference=/etc/legacy-fsd/local.crt\n' > /etc/config/uhttpd
sha256sum /etc/legacy-fsd/config.yaml /etc/legacy-fsd/local.crt /etc/config/uhttpd > /tmp/preserve.sha
mkdir /etc/fsd
if sh /work/files/preinst; then echo 'collision was accepted'; exit 1; fi
[ ! -f /tmp/legacy-service-calls ]
rmdir /etc/fsd
sh /work/files/preinst
[ "$(readlink /etc/legacy-fsd)" = /etc/fsd ]
[ "$(readlink /etc/fsd/config.yaml)" = profiles/default.yaml ]
sha256sum -c /tmp/preserve.sha
[ "$(cat /tmp/legacy-service-calls)" = "$(printf 'stop\ndisable')" ]
sh /work/files/preinst
sha256sum -c /tmp/preserve.sha
[ -f /etc/fsd-package/name-backup.*/config.tar.gz ]
printf '/etc/init.d/fsd-service\n' > /lib/apk/packages/luci-app-fsd.list
sh /work/files/preinst
[ "$(wc -l < /tmp/legacy-service-calls)" -eq 4 ]
echo 'PASS: collision protection, dynamic discovery, private backup, profile links, certificate references, repeat migration and current-layout no-op'
