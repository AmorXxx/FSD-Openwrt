#!/bin/sh
# Run ONLY in a disposable OpenWrt container; network/service operations are stubbed.
set -eu
mkdir -p /tmp/fsd-test/bin /var/lock /var/run /etc/nftables.d /etc/config
export PATH="/tmp/fsd-test/bin:$PATH"
for tool in fw4 nft iw conntrack; do
	printf '#!/bin/sh\nexit 0\n' > "/tmp/fsd-test/bin/$tool"
done
cat > /tmp/fsd-test/bin/ip <<'EOF'
#!/bin/sh
echo '    inet 192.0.2.1/24 scope global br-lan'
EOF
cat > /tmp/fsd-test/bin/ubus <<'EOF'
#!/bin/sh
echo '{"ok": true, "results": [{"status": "pass"}]}'
EOF
cat > /tmp/fsd-test/bin/curl <<'EOF'
#!/bin/sh
exit 0
EOF
cat > /tmp/fsd-test/bin/netstat <<'EOF'
#!/bin/sh
echo 'tcp 0 0 0.0.0.0:443 0.0.0.0:* LISTEN 12/uhttpd'
echo 'udp 0 0 0.0.0.0:53 0.0.0.0:* 13/dnsmasq'
EOF
chmod 755 /tmp/fsd-test/bin/*
for name in fsd-service firewall dnsmasq uhttpd rpcd; do
	cat > "/etc/init.d/$name" <<'EOF'
#!/bin/sh
if [ "${FSD_TEST_FAIL_RELOAD:-0}" = 1 ] && [ "$1" = reload ]; then exit 1; fi
exit 0
EOF
	chmod 755 "/etc/init.d/$name"
done
cat > /etc/config/network <<'EOF'
config interface 'lan'
 option device 'br-lan'
 option proto 'static'
config interface 'wan'
 option proto 'mu300cell'
EOF
cat > /etc/config/wireless <<'EOF'
config wifi-iface 'ap0'
 option mode 'ap'
EOF
cat > /etc/config/dhcp <<'EOF'
config dnsmasq
 option port '53'
 option domain 'lan'
config dhcp 'lan'
 list dhcp_option '6,192.0.2.2'
 list dhcp_option '42,192.0.2.3'
EOF
cat > /etc/config/firewall <<'EOF'
config defaults
 option flow_offloading '1'
 option flow_offloading_hw '0'
EOF
cat > /etc/config/uhttpd <<'EOF'
config uhttpd 'main'
 list listen_https '0.0.0.0:443'
 list listen_https '[::]:443'
 option cert '/etc/uhttpd.crt'
 option key '/etc/uhttpd.key'
EOF

fsd-setup check
fsd-setup enable
[ -f /etc/fsd-package/active ]
[ "$(uci get 'dhcp.@dnsmasq[0].port')" = 5355 ]
[ "$(uci get dhcp.lan.dhcp_option)" = '42,192.0.2.3 6,192.0.2.1' ]
[ "$(uci get uhttpd.main.listen_https)" = '0.0.0.0:8444' ]
[ "$(uci get 'firewall.@defaults[0].flow_offloading')" = 0 ]
sha256sum /etc/fsd/config.yaml /etc/fsd/local.key > /tmp/fsd-test/preserved.sha256
fsd-setup enable
sha256sum -c /tmp/fsd-test/preserved.sha256
uci set 'dhcp.@dnsmasq[0].domain=edited.lan'
uci commit dhcp
fsd-setup disable
[ ! -e /etc/fsd-package/active ]
[ ! -e /etc/nftables.d/45-fsd-forward-guard.nft ]
[ "$(uci get 'dhcp.@dnsmasq[0].port')" = 53 ]
[ "$(uci get 'dhcp.@dnsmasq[0].domain')" = edited.lan ]
[ "$(uci get dhcp.lan.dhcp_option)" = '6,192.0.2.2 42,192.0.2.3' ]
[ "$(uci get uhttpd.main.listen_https)" = '0.0.0.0:443 [::]:443' ]
[ "$(uci get 'firewall.@defaults[0].flow_offloading')" = 1 ]
sha256sum -c /tmp/fsd-test/preserved.sha256
echo 'PASS: activation, idempotence, disable, unrelated edits and secrets preserved'

sha256sum /etc/config/dhcp /etc/config/firewall /etc/config/uhttpd > /tmp/fsd-test/before-failure.sha256
if FSD_TEST_FAIL_RELOAD=1 fsd-setup enable; then
	echo 'Expected activation failure' >&2; exit 1
fi
sha256sum -c /tmp/fsd-test/before-failure.sha256
[ ! -e /etc/fsd-package/active ]
[ ! -e /etc/nftables.d/45-fsd-forward-guard.nft ]
echo 'PASS: injected activation failure restores all touched network settings'

fsd-setup enable
uci set 'uhttpd.main.cert=/etc/custom.crt'
uci commit uhttpd
fsd-setup disable
[ "$(uci get uhttpd.main.cert)" = /etc/custom.crt ]
echo 'PASS: later managed-option edits are preserved on disable'
