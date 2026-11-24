#!/bin/sh
set -eu
cleanup() {
 result=$?
 if [ "$result" != 0 ]; then
 docker logs fsd-scope-router || true
 docker logs fsd-scope-wan || true
 docker exec fsd-scope-router ip addr || true
 docker exec fsd-scope-router ip route || true
 docker exec fsd-scope-router nft list ruleset || true
 docker exec fsd-scope-wan ip route || true
 fi
 docker rm -f fsd-scope-router fsd-scope-wan >/dev/null 2>&1 || true
 docker network rm fsd-scope-lan fsd-scope-wan >/dev/null 2>&1 || true
}
for name in fsd-scope-lan fsd-scope-wan; do
 if docker network inspect "$name" >/dev/null 2>&1; then echo "Test network already exists: $name" >&2; exit 1; fi
done
for name in fsd-scope-router fsd-scope-wan; do
 if docker container inspect "$name" >/dev/null 2>&1; then echo "Test container already exists: $name" >&2; exit 1; fi
done
test_binary=${FSD_SCOPE_TEST_BINARY:-/tmp/fsd-scope.test}
[ -f "$test_binary" ] || { echo "Build a Linux ARM64 Go test binary first: $test_binary" >&2; exit 1; }
trap cleanup EXIT
# All networks and containers here are disposable test fixtures.
docker network create --ipv6 --subnet 172.30.40.0/24 --subnet fd73:30:40::/64 fsd-scope-lan >/dev/null
docker network create --ipv6 --subnet 172.30.41.0/24 --subnet fd73:30:41::/64 fsd-scope-wan >/dev/null
docker run -d --name fsd-scope-router --network fsd-scope-lan --ip 172.30.40.2 --ip6 fd73:30:40::2 --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 --sysctl net.ipv6.conf.all.forwarding=1 -e FSD_SCOPE_TRAFFIC=server -v "$test_binary:/test:ro" alpine:edge sh -ec 'apk add --no-cache nftables >/dev/null; /test -test.run "^TestScopeTrafficServer$" -test.v' >/dev/null
docker network connect --ip 172.30.41.2 --ip6 fd73:30:41::2 fsd-scope-wan fsd-scope-router
docker run -d --name fsd-scope-wan --network fsd-scope-wan --ip 172.30.41.10 --ip6 fd73:30:41::10 --cap-add NET_ADMIN alpine:edge sh -ec 'ip route add 172.30.40.0/24 via 172.30.41.2; ip -6 route add fd73:30:40::/64 via fd73:30:41::2; apk add --no-cache busybox-extras >/dev/null; mkdir /www; echo ok > /www/index.html; busybox-extras httpd -f -p 8081 -h /www' >/dev/null
n=0
until docker exec fsd-scope-router wget -qO- http://127.0.0.1:8080/ready >/dev/null 2>&1; do
 n=$((n+1)); [ "$n" -lt 40 ] || { docker logs fsd-scope-router; exit 1; }; sleep 1
done
n=0
until docker exec fsd-scope-wan wget -qO- http://127.0.0.1:8081/ >/dev/null 2>&1; do
 n=$((n+1)); [ "$n" -lt 40 ] || { docker logs fsd-scope-wan; exit 1; }; sleep 1
done
client() {
 docker run --rm --network fsd-scope-lan --ip "172.30.40.$1" --ip6 "fd73:30:40::$1" --cap-add NET_ADMIN -e FSD_SCOPE_TRAFFIC=client -e "FSD_EXPECT_DNS=$2" -e "FSD_EXPECT_WAN=$3" -v "$test_binary:/test:ro" alpine:edge sh -ec 'ip route add 172.30.41.0/24 via 172.30.40.2; ip -6 route add fd73:30:41::/64 via fd73:30:40::2; /test -test.run "^TestScopeTrafficClient$" -test.v'
}
client 10 192.0.2.1 open
client 20 203.0.113.9 open
docker exec fsd-scope-router wget -qO- http://127.0.0.1:8080/block
client 10 192.0.2.1 blocked
client 20 203.0.113.9 open
docker exec fsd-scope-router wget -qO- http://127.0.0.1:8080/unblock
docker exec fsd-scope-router wget -qO- http://127.0.0.1:8080/stop
client 10 timeout blocked
client 20 203.0.113.9 open
docker exec fsd-scope-router wget -qO- http://127.0.0.1:8080/finish
[ "$(docker wait fsd-scope-router)" = 0 ]
printf '\nPASS: IPv4/IPv6 target/non-target DNS (UDP/TCP), JSON, IP blocks and service-stop isolation\n'
