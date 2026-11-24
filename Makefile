include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-fsd
PKG_VERSION:=1.7.1
PKG_RELEASE:=1
PKG_LICENSE:=LicenseRef-Unspecified
PKG_BUILD_DIR:=$(BUILD_DIR)/$(PKG_NAME)-$(PKG_VERSION)

include $(INCLUDE_DIR)/package.mk

define Package/luci-app-fsd
  SECTION:=luci
  CATEGORY:=LuCI
  SUBMENU:=3. Applications
  TITLE:=FSD DNS and HTTPS JSON service
  DEPENDS:=@aarch64 +firewall4 +nftables +dnsmasq +uhttpd +uhttpd-mod-ubus +luci-ssl +rpcd +rpcd-mod-iwinfo +curl +ca-bundle +iw +conntrack
endef

define Package/luci-app-fsd/description
  FSD service and LuCI UI for ARM64 OpenWrt. Run fsd-setup enable after installation.
endef

# Build dist/fsd-service-linux-arm64 with scripts/build.py before invoking the SDK.
define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) ./files $(PKG_BUILD_DIR)/
	$(CP) ./dist/fsd-service-linux-arm64 $(PKG_BUILD_DIR)/fsd-service
endef
define Build/Compile
endef

define Package/luci-app-fsd/conffiles
/etc/config/fsd
/etc/fsd/
/etc/fsd-package/
/etc/nftables.d/45-fsd-forward-guard.nft
endef

define Package/luci-app-fsd/install
	$(INSTALL_DIR) $(1)/usr/sbin $(1)/etc/init.d $(1)/etc/config $(1)/usr/share/fsd
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/fsd-service $(1)/usr/sbin/fsd-service
	$(INSTALL_BIN) ./files/fsd-setup $(1)/usr/sbin/fsd-setup
	$(INSTALL_BIN) ./files/fsd-service.init $(1)/etc/init.d/fsd-service
	$(INSTALL_CONF) ./files/fsd.config $(1)/etc/config/fsd
	$(INSTALL_DATA) ./files/config.example.yaml ./files/fsd-forward-guard.nft $(1)/usr/share/fsd/
	echo '$(PKG_VERSION)-r$(PKG_RELEASE)' > $(1)/usr/share/fsd/package-version
	$(INSTALL_DIR) $(1)/usr/share/luci/menu.d $(1)/usr/share/rpcd/acl.d $(1)/usr/libexec/rpcd
	$(INSTALL_DATA) ./files/luci/menu.json $(1)/usr/share/luci/menu.d/luci-app-fsd.json
	$(INSTALL_DATA) ./files/luci/acl.json $(1)/usr/share/rpcd/acl.d/luci-app-fsd.json
	$(INSTALL_BIN) ./files/luci/rpcd-fsd $(1)/usr/libexec/rpcd/fsd
	$(INSTALL_DIR) $(1)/www/luci-static/resources/view/fsd
	$(INSTALL_DATA) ./files/luci/overview.js $(1)/www/luci-static/resources/view/fsd/overview-v2.js
	$(INSTALL_DATA) ./files/luci/config-editor.js $(1)/www/luci-static/resources/view/fsd/config-editor-v2.js
	$(INSTALL_DATA) ./files/luci/overview.css $(1)/www/luci-static/resources/view/fsd/overview.css
	$(INSTALL_DIR) $(1)/usr/share/fsd/hooks
	$(INSTALL_BIN) ./files/postinst ./files/prerm ./files/postrm $(1)/usr/share/fsd/hooks/
	$(INSTALL_DIR) $(1)/usr/share/licenses/luci-app-fsd
	$(INSTALL_DATA) ./THIRD_PARTY_LICENSES/* $(1)/usr/share/licenses/luci-app-fsd/
endef

define Package/luci-app-fsd/preinst
#!/bin/sh
set -eu
[ -z "$${IPKG_INSTROOT:-}" ] || exit 0
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
	if [ -f "$$candidate" ]; then inventory=$$candidate; break; fi
done
[ -n "$$inventory" ] || exit 0
previous_service=$$(sed -n '\|^/etc/init.d/[^/]*$$|p' "$$inventory")
[ -n "$$previous_service" ] || exit 0
[ "$$previous_service" != /etc/init.d/fsd-service ] || exit 0
[ "$$(printf '%s\n' "$$previous_service" | wc -l)" -eq 1 ] || { echo 'Ambiguous FSD service inventory' >&2; exit 1; }
[ -f "$$previous_service" ] || { echo 'Previous FSD service file missing' >&2; exit 1; }
previous_data=$$(sed -n 's|.*< \(/etc/[^/ ]*\)/lan-prefix.*|\1|p' "$$previous_service")
[ -n "$$previous_data" ] && [ "$$(printf '%s\n' "$$previous_data" | wc -l)" -eq 1 ] || { echo 'Cannot determine previous FSD configuration directory' >&2; exit 1; }
[ -f "$$previous_data/config.yaml" ] && [ -f "$$previous_data/lan-prefix" ] || { echo 'Previous FSD configuration incomplete' >&2; exit 1; }
if [ "$$previous_data" != /etc/fsd ] && [ "$$(readlink "$$previous_data" 2>/dev/null || true)" != /etc/fsd ]; then
	[ ! -e /etc/fsd ] && [ ! -L /etc/fsd ] || { echo '/etc/fsd already exists; resolve configuration collision before upgrading' >&2; exit 1; }
	umask 077
	mkdir -p /etc/fsd-package
	backup=$$(mktemp -d /etc/fsd-package/name-backup.XXXXXX)
	tar -czf "$$backup/config.tar.gz" "$$previous_data" /etc/config/uhttpd
	cp -p "$$previous_service" "$$backup/service.init"
	mv "$$previous_data" /etc/fsd
	# Existing certificate references and management listeners remain valid.
	if ! ln -s /etc/fsd "$$previous_data"; then mv /etc/fsd "$$previous_data"; exit 1; fi
	mkdir -p /lib/upgrade/keep.d
	printf '%s\n' "$$previous_data" /lib/upgrade/keep.d/fsd-compat > /lib/upgrade/keep.d/fsd-compat
fi
"$$previous_service" stop
"$$previous_service" disable
endef

define Package/luci-app-fsd/postinst
#!/bin/sh
[ -n "$$IPKG_INSTROOT" ] && exit 0
/usr/share/fsd/hooks/postinst
endef

define Package/luci-app-fsd/prerm
#!/bin/sh
[ -n "$$IPKG_INSTROOT" ] && exit 0
/usr/share/fsd/hooks/prerm "$$@"
endef

# Inline postrm since installed hook files have already been removed.
define Package/luci-app-fsd/postrm
#!/bin/sh
[ -n "$$IPKG_INSTROOT" ] && exit 0
rm -f /tmp/luci-indexcache /tmp/luci-indexcache.*
/etc/init.d/rpcd reload || true
endef

$(eval $(call BuildPackage,luci-app-fsd))
