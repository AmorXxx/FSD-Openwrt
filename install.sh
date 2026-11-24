#!/bin/sh
# Run on the router with the package files in ./dist (or pass an explicit file).
set -eu
[ "$(id -u)" = 0 ] || { echo 'Run as root' >&2; exit 1; }
[ "$(uname -m)" = aarch64 ] || { echo 'This build requires ARM64' >&2; exit 1; }
if [ -e /usr/sbin/fsd-service ] && [ ! -f /usr/share/fsd/package-version ]; then
	echo 'Manual FSD installation detected. No package-manager command has been run.' >&2
	echo 'Follow README migration steps first; APK script failures do not guarantee aborting a transaction.' >&2
	exit 1
fi
base=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
migration_preflight() (
set -eu
[ -z "${IPKG_INSTROOT:-}" ] || exit 0
# Package-owned installs include this marker; unowned manual installs must be migrated.
if [ -e /usr/sbin/fsd-service ] && [ ! -f /usr/share/fsd/package-version ]; then
	echo 'Existing manual FSD installation found. Back it up and follow README migration instructions before installing this package.' >&2
	exit 1
fi

# Discover the previous package layout from its installed inventory, without
# embedding historical service names in the current source tree.
[ -f /usr/share/fsd/package-version ] || exit 0
inventory=
for candidate in /lib/apk/packages/luci-app-fsd.list /usr/lib/opkg/info/luci-app-fsd.list; do
	if [ -f "$candidate" ]; then inventory=$candidate; break; fi
done
[ -n "$inventory" ] || exit 0
previous_service=$(sed -n '\|^/etc/init.d/[^/]*$|p' "$inventory")
[ -n "$previous_service" ] || exit 0
[ "$previous_service" != /etc/init.d/fsd-service ] || exit 0
[ "$(printf '%s\n' "$previous_service" | wc -l)" -eq 1 ] || { echo 'Ambiguous FSD service inventory' >&2; exit 1; }
[ -f "$previous_service" ] || { echo 'Previous FSD service file missing' >&2; exit 1; }
previous_data=$(sed -n 's|.*< \(/etc/[^/ ]*\)/lan-prefix.*|\1|p' "$previous_service")
[ -n "$previous_data" ] && [ "$(printf '%s\n' "$previous_data" | wc -l)" -eq 1 ] || { echo 'Cannot determine previous FSD configuration directory' >&2; exit 1; }
[ -f "$previous_data/config.yaml" ] && [ -f "$previous_data/lan-prefix" ] || { echo 'Previous FSD configuration incomplete' >&2; exit 1; }
if [ "$previous_data" != /etc/fsd ] && [ "$(readlink "$previous_data" 2>/dev/null || true)" != /etc/fsd ]; then
	[ ! -e /etc/fsd ] && [ ! -L /etc/fsd ] || { echo '/etc/fsd already exists; resolve configuration collision before upgrading' >&2; exit 1; }
	umask 077
	mkdir -p /etc/fsd-package
	backup=$(mktemp -d /etc/fsd-package/name-backup.XXXXXX)
	tar -czf "$backup/config.tar.gz" "$previous_data" /etc/config/uhttpd
	cp -p "$previous_service" "$backup/service.init"
	mv "$previous_data" /etc/fsd
	# Existing certificate references and management listeners remain valid.
	if ! ln -s /etc/fsd "$previous_data"; then mv /etc/fsd "$previous_data"; exit 1; fi
	mkdir -p /lib/upgrade/keep.d
	printf '%s\n' "$previous_data" /lib/upgrade/keep.d/fsd-compat > /lib/upgrade/keep.d/fsd-compat
fi
"$previous_service" stop
"$previous_service" disable

)

if command -v apk >/dev/null; then
	file=${1:-}
	if [ -z "$file" ]; then
		# OpenWrt may override the binary's generic architecture in this file.
		if [ -s /etc/apk/arch ]; then
			read -r arch < /etc/apk/arch
		else
			arch=$(apk --print-arch) || { echo 'Cannot read APK architecture' >&2; exit 1; }
		fi
		case "$arch" in aarch64_*) ;; *) echo "Unsupported APK architecture: $arch" >&2; exit 1 ;; esac
		file="$base/dist/luci-app-fsd-1.7.4-r1_${arch}.apk"
	fi
	[ -f "$file" ] || { echo "Missing package: $file" >&2; exit 1; }
	migration_preflight
	apk add --allow-untrusted "$file"
elif command -v opkg >/dev/null; then
	file=${1:-}
	if [ -z "$file" ]; then
		arches=$(opkg print-architecture) || { echo 'Cannot read opkg architectures' >&2; exit 1; }
		for arch in $(printf '%s\n' "$arches" | sort -k3,3nr | awk '$1 == "arch" && $2 ~ /^aarch64_[a-z0-9_-]+$/ {print $2}'); do
			candidate="$base/dist/luci-app-fsd_1.7.4-1_${arch}.ipk"
			if [ -f "$candidate" ]; then file=$candidate; break; fi
		done
		[ -n "$file" ] || { echo 'No package matches the architectures accepted by opkg; obtain a matching build.' >&2; exit 1; }
	fi
	[ -f "$file" ] || { echo "Missing package: $file" >&2; exit 1; }
	migration_preflight
	opkg install "$file"
else
	echo 'Neither apk nor opkg is available' >&2
	exit 1
fi
