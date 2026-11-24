package main

import "testing"

func TestConntrackExtendedOutput(t *testing.T) {
	for _, line := range []string{
		"ipv4 2 tcp 6 120 ESTABLISHED src=192.168.50.200 dst=1.1.1.1 sport=12345 dport=443 packets=3 bytes=500 src=1.1.1.1 dst=10.0.0.1 sport=443 dport=12345 packets=4 bytes=900 mark=0 use=1",
		"ipv6 10 tcp 6 120 ESTABLISHED src=fd00::2 dst=2606:4700::1111 sport=12345 dport=443 packets=3 bytes=500 src=2606:4700::1111 dst=fd00::2 sport=443 dport=12345 packets=4 bytes=900 mark=0 use=1",
	} {
		got, ok := parseConnection(line)
		if !ok || got.Protocol != "TCP" || got.SourcePort != "12345" || got.Port != "443" || got.Up != 500 || got.Down != 900 {
			t.Fatalf("bad extended conntrack parse: %+v, %v", got, ok)
		}
	}
}
