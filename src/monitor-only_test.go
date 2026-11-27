package main

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/miekg/dns"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func monitorConfig(t *testing.T) config {
	t.Helper()
	data, err := os.ReadFile("../files/仅监控.yaml")
	// Test executables also run in isolated containers without the source tree.
	if err != nil {
		data = []byte("version: 1\nmonitor_only: true\ndevice_scope: {mode: all, addresses: []}\ndns_rules: []\n")
	}
	cfg, err := decodeConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
func TestMonitorOnlyValidation(t *testing.T) {
	cfg := monitorConfig(t)
	if !cfg.MonitorOnly || len(cfg.sites()) != 0 {
		t.Fatal("monitor mode not decoded")
	}
	for _, mutate := range []func(*config){
		func(c *config) { c.Scope = deviceScope{Mode: "selected", Addresses: []string{"192.0.2.10"}} },
		func(c *config) { c.DNS = []dnsRecord{{Name: "x.example", IP: "ap"}} },
		func(c *config) { c.Sites = []siteConfig{{Domain: "x.example"}} },

		func(c *config) { c.Blacklist.Block = []string{"example"} },
	} {
		bad := cfg
		mutate(&bad)
		if bad.validate() == nil {
			t.Fatal("accepted interception fields in monitor mode")
		}
	}
}
func TestMonitorOnlyForwardAndLog(t *testing.T) {
	app := scopeTestApp(t)
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(q)
		if q.Question[0].Qtype == dns.TypeAAAA {
			r.Answer = []dns.RR{&dns.AAAA{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 20}, AAAA: net.ParseIP("2001:db8::9")}}
		} else {
			r.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 20}, A: net.ParseIP("203.0.113.9")}}
		}
		_ = w.WriteMsg(r)
	})}
	go server.ActivateAndServe()
	defer server.Shutdown()
	app.dnsUpstream = packet.LocalAddr().String()
	cfg := monitorConfig(t)
	next, err := app.prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	app.current.Store(next)
	app.monitor.blocked = []string{"example"}
	for _, ip := range []string{"192.0.2.10", "192.0.2.20", "fd00::20", "127.0.0.1"} {
		for _, typ := range []uint16{dns.TypeA, dns.TypeAAAA} {
			w := &scopeDNSWriter{remote: &net.UDPAddr{IP: net.ParseIP(ip), Port: 5000}}
			q := new(dns.Msg)
			q.SetQuestion("blocked.example.", typ)
			app.serveDNS(w, q)
			if w.reply == nil || w.reply.Rcode != dns.RcodeSuccess || len(w.reply.Answer) != 1 {
				t.Fatal("monitor request intercepted", ip, typ)
			}
		}
	}
	if len(app.monitor.events) != 8 {
		t.Fatal("missing DNS logs")
	}
	for _, event := range app.monitor.events {
		if event.Result != "forwarded" {
			t.Fatal("wrong result", event)
		}
	}
	if blockedName("blocked.example", cfg, app.monitor.blocked) {
		t.Fatal("temporary blacklist applied")
	}
	if strings.Contains(app.blocklistScript(cfg), "add element") {
		t.Fatal("IP blocking populated")
	}
	if len(next.records) != 0 {
		t.Fatal("monitor mode populated DNS overrides")
	}
	for _, sni := range []string{"", "blocked.example", "fsd-monitor.home.arpa"} {
		if _, err = app.getCertificate(&tls.ClientHelloInfo{ServerName: sni}); err == nil {
			t.Fatal("business TLS accepted")
		}
	}
	for _, path := range []string{"/", "/a", "/status", "/headers", "/echo"} {
		w := httptest.NewRecorder()
		app.publicSite(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 || strings.Contains(w.Header().Get("Content-Type"), "json") {
			t.Fatal("business endpoint replied", path)
		}
	}
	checks := app.checkDNS(context.Background(), next)
	if checks[len(checks)-1].Status != "skip" {
		t.Fatal("blacklist self-check not skipped")
	}
}
func TestMonitorOnlyFirewallTransitions(t *testing.T) {
	if os.Getenv("FSD_TEST_NFT") != "1" {
		t.Skip("isolated nftables container only")
	}
	app := profileTestApp(t)
	original := app.current.Load()
	cfg := original.config
	cfg.Scope = deviceScope{Mode: "all"}
	original = app.runtimeSettings(cfg, original.tls, original.certificates)
	app.current.Store(original)
	app.firewallEnabled = true
	app.guardEnabled = true
	guard := "/etc/nftables.d/45-fsd-forward-guard.nft"
	os.MkdirAll(filepath.Dir(guard), 0700)
	_ = nft("delete table inet fw4\ndelete table inet fsd_service")
	if err := nft("table inet fw4 {\n" + string(app.guardFile(cfg.Scope)) + "}\n" + app.filterScript(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(guard, app.guardFile(cfg.Scope)); err != nil {
		t.Fatal(err)
	}
	next, err := app.prepare(monitorConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = app.commitSettings(next, func() error { return errors.New("injected") }); err == nil {
		t.Fatal("expected rollback")
	}
	snapshot, _ := exec.Command("nft", "-j", "list", "table", "inet", "fw4").Output()
	if app.current.Load() != original || inspectForwardGuard(snapshot, app.lanDevice, cfg.Scope).Status != "pass" {
		t.Fatal("rollback failed")
	}
	if err = app.commitSettings(next, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = exec.Command("nft", "-j", "list", "table", "inet", "fw4").Output()
	if inspectMonitorGuard(snapshot).Status != "pass" {
		t.Fatalf("monitor guard not cleared: %s", snapshot)
	}
	boot, _ := os.ReadFile(guard)
	if strings.Contains(string(boot), "counter drop") || strings.Contains(string(boot), "redirect to") {
		t.Fatal("boot guard still intercepts")
	}
	// Simulate service stop: persistent monitor guard does not depend on the lease.
	if err = nft(clearForwardReady + "delete table inet fsd_service\n"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = exec.Command("nft", "-j", "list", "table", "inet", "fw4").Output()
	if inspectMonitorGuard(snapshot).Status != "pass" {
		t.Fatal("stop enabled guard")
	}
	if err = app.commitSettings(original, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = exec.Command("nft", "-j", "list", "table", "inet", "fw4").Output()
	if inspectForwardGuard(snapshot, app.lanDevice, cfg.Scope).Status != "pass" {
		t.Fatal("normal guard not restored")
	}
}

func TestMonitorModePreservesExistingCertificate(t *testing.T) {
	app := scopeTestApp(t)
	original := app.current.Load().config
	if _, err := app.prepare(original); err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(filepath.Join(app.dir, "local.crt"))
	key, _ := os.ReadFile(filepath.Join(app.dir, "local.key"))
	if _, err := app.prepare(monitorConfig(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.prepare(original); err != nil {
		t.Fatal(err)
	}
	afterCert, _ := os.ReadFile(filepath.Join(app.dir, "local.crt"))
	afterKey, _ := os.ReadFile(filepath.Join(app.dir, "local.key"))
	if string(cert) != string(afterCert) || string(key) != string(afterKey) {
		t.Fatal("monitor switch replaced existing certificate")
	}
}
