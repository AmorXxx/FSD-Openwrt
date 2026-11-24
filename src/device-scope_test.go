package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func scopeTestApp(t *testing.T) *application {
	t.Helper()
	app := &application{dir: t.TempDir(), lanAddr: netip.MustParseAddr("192.0.2.1"), lanDevice: "br-lan", dnsPort: 53, resolverPort: 5355, sitePort: 443, adminPort: 8444, monitor: newMonitor()}
	cfg := config{Version: 1, Site: siteConfig{Domain: "site.home.arpa"}, Scope: deviceScope{Mode: "selected", Addresses: []string{"192.0.2.10", "fd00::10"}}, Routes: []customRoute{{Path: "/a", JSON: `{"ok":true}`}}, Blacklist: dnsBlacklist{Block: []string{"blocked.example"}}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	app.current.Store(app.runtimeSettings(cfg, &tls.Certificate{}))
	return app
}
func TestDeviceScopeValidationAndHTTP(t *testing.T) {
	defaultScope := deviceScope{}
	if err := defaultScope.validate(); err != nil || defaultScope.Mode != "all" {
		t.Fatal("legacy default changed")
	}
	linkLocal := deviceScope{Mode: "selected", Addresses: []string{"fe80::10"}}
	if !linkLocal.selected(netip.MustParseAddr("fe80::10%br-lan")) {
		t.Fatal("IPv6 scope zone should not change device identity")
	}

	for _, s := range []deviceScope{{Mode: "invalid"}, {Mode: "selected"}, {Mode: "selected", Addresses: []string{"127.0.0.1"}}, {Addresses: []string{"192.0.2.0/24"}}, {Addresses: []string{"fd00::1%br-lan"}}, {Addresses: []string{"192.0.2.2", "::ffff:192.0.2.2"}}, {Addresses: make([]string, 65)}} {
		if s.validate() == nil {
			t.Fatalf("accepted invalid scope %+v", s)
		}
	}
	app := scopeTestApp(t)
	for _, tc := range []struct {
		source string
		want   int
	}{{"192.0.2.10:123", 200}, {"[fd00::10]:123", 200}, {"192.0.2.20:123", 404}, {"[fd00::20]:123", 404}, {"127.0.0.1:123", 200}, {"192.0.2.1:123", 200}} {
		req := httptest.NewRequest("GET", "https://site.home.arpa/a", nil)
		req.RemoteAddr = tc.source
		w := httptest.NewRecorder()
		app.publicSite(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s: got %d", tc.source, w.Code)
		}
	}
}

type scopeDNSWriter struct {
	remote net.Addr
	reply  *dns.Msg
}

func (w *scopeDNSWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 53}
}
func (w *scopeDNSWriter) RemoteAddr() net.Addr        { return w.remote }
func (w *scopeDNSWriter) WriteMsg(m *dns.Msg) error   { w.reply = m; return nil }
func (w *scopeDNSWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *scopeDNSWriter) Close() error                { return nil }
func (w *scopeDNSWriter) TsigStatus() error           { return nil }
func (w *scopeDNSWriter) TsigTimersOnly(bool)         {}
func (w *scopeDNSWriter) Hijack()                     {}
func TestDeviceScopeDNS(t *testing.T) {
	app := scopeTestApp(t)
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(q)
		r.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 10}, A: net.ParseIP("203.0.113.9")}}
		_ = w.WriteMsg(r)
	})}
	go server.ActivateAndServe()
	defer server.Shutdown()
	app.dnsUpstream = packet.LocalAddr().String()
	for _, tc := range []struct {
		client, name, ip string
		rcode            int
	}{{"192.0.2.10", "site.home.arpa", "192.0.2.1", 0}, {"192.0.2.20", "site.home.arpa", "203.0.113.9", 0}, {"fd00::20", "blocked.example", "203.0.113.9", 0}, {"fd00::10", "blocked.example", "", dns.RcodeNameError}, {"192.0.2.10", "other.example", "203.0.113.9", 0}} {
		w := &scopeDNSWriter{remote: &net.UDPAddr{IP: net.ParseIP(tc.client), Port: 3000}}
		q := new(dns.Msg)
		q.SetQuestion(dns.Fqdn(tc.name), dns.TypeA)
		app.serveDNS(w, q)
		if w.reply == nil || w.reply.Rcode != tc.rcode {
			t.Fatalf("DNS result %+v", tc)
		}
		if tc.ip != "" && (len(w.reply.Answer) != 1 || w.reply.Answer[0].(*dns.A).A.String() != tc.ip) {
			t.Fatalf("DNS address %+v", tc)
		}
	}
}
func TestDeviceScopeEditor(t *testing.T) {
	app := scopeTestApp(t)
	before := app.current.Load()
	input := editorUpdate{Revision: snapshotForEditor(before.config).Revision, DNS: []dnsRecord{}, Routes: []editorRouteUpdate{{Path: "/a", Source: "/a"}}, Scope: &deviceScope{Mode: "selected", Addresses: []string{"192.0.2.20"}}}
	saved, err := app.saveEditor(input)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Scope.Addresses[0] != "192.0.2.20" || before.Scope.Addresses[0] != "192.0.2.10" {
		t.Fatal("scope update mutated old config")
	}
	raw, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	loaded, err := decodeConfig(raw)
	if err != nil || !sameScope(loaded.Scope, saved.Scope) {
		t.Fatal("scope not persisted")
	}
	if _, err = app.saveEditor(input); err == nil {
		t.Fatal("stale scope accepted")
	}
	input.Revision = saved.Revision
	input.Scope = nil
	if _, err = app.saveEditor(input); err != nil || app.current.Load().Scope.Mode != "selected" {
		t.Fatal("old client erased scope")
	}
	input.Scope = &deviceScope{Mode: "selected"}
	if _, err = app.saveEditor(input); err == nil {
		t.Fatal("empty selected scope accepted")
	}
}

// Run only in a disposable network namespace/container with CAP_NET_ADMIN.
func TestDeviceScopeRealNft(t *testing.T) {
	if os.Getenv("FSD_TEST_NFT") != "1" {
		t.Skip("requires isolated Linux nftables container")
	}
	app := scopeTestApp(t)
	for _, scope := range []deviceScope{{Mode: "all"}, app.current.Load().Scope, {Mode: "selected", Addresses: []string{"192.0.2.11"}}, {Mode: "selected", Addresses: []string{"fd00::11"}}, {Mode: "all"}} {
		_ = nft("delete table inet fw4\ndelete table inet fsd_service\n")
		if err := nft("table inet fw4 {\n" + string(app.guardFile(scope)) + "}\n"); err != nil {
			t.Fatal(err)
		}
		cfg := app.current.Load().config
		cfg.Scope = scope
		if err := nft(app.filterScript(cfg) + app.liveGuard(scope)); err != nil {
			t.Fatal(err)
		}
		data, err := exec.Command("nft", "-j", "list", "table", "inet", "fw4").Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := inspectForwardGuard(data, app.lanDevice, scope); got.Status != "pass" {
			t.Fatalf("diagnostic: %+v\n%s", got, data)
		}
		// Simulate service shutdown: permanent DNS bypass and scoped boot guard survive.
		if err := nft("delete table inet fsd_service"); err != nil {
			t.Fatal(err)
		}
		data, _ = exec.Command("nft", "list", "chain", "inet", "fw4", "fsd_dns_bypass").Output()
		if scope.Mode == "selected" && !strings.Contains(string(data), "redirect to :5355") {
			t.Fatal("DNS bypass missing after stop")
		}
	}
}

func TestDeviceScopeCommitRollbackNft(t *testing.T) {
	if os.Getenv("FSD_TEST_NFT") != "1" {
		t.Skip("isolated nftables container only")
	}
	app := scopeTestApp(t)
	app.firewallEnabled = true
	app.guardEnabled = true
	guard := "/etc/nftables.d/45-fsd-forward-guard.nft"
	if err := os.MkdirAll(filepath.Dir(guard), 0700); err != nil {
		t.Fatal(err)
	}
	_ = nft("delete table inet fw4\ndelete table inet fsd_service")
	if err := nft("table inet fw4 {\n" + string(app.guardFile(app.current.Load().Scope)) + "}\n" + app.filterScript(app.current.Load().config)); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(guard, app.guardFile(app.current.Load().Scope)); err != nil {
		t.Fatal(err)
	}
	initial := app.current.Load()
	if err := app.commitSettings(initial, nil); err != nil {
		t.Fatal(err)
	}
	oldFile, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	oldGuard, _ := os.ReadFile(guard)
	cfg := initial.config
	cfg.Scope = deviceScope{Mode: "selected", Addresses: []string{"192.0.2.22", "fd00::22"}}
	next := app.runtimeSettings(cfg, initial.tls)
	if err := app.commitSettings(next, func() error { return errors.New("injected failure") }); err == nil {
		t.Fatal("expected rollback")
	}
	afterFile, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	afterGuard, _ := os.ReadFile(guard)
	if app.current.Load() != initial || !bytes.Equal(oldFile, afterFile) || !bytes.Equal(oldGuard, afterGuard) {
		t.Fatal("rollback failed to preserve file/state")
	}
	snapshot, _ := exec.Command("nft", "-j", "list", "table", "inet", "fw4").Output()
	if result := inspectForwardGuard(snapshot, app.lanDevice, initial.Scope); result.Status != "pass" {
		t.Fatalf("live rollback failed: %+v", result)
	}
	if err := app.commitSettings(next, nil); err != nil {
		t.Fatal(err)
	}
	if app.current.Load() != next {
		t.Fatal("new scope not activated")
	}
	persisted, _ := os.ReadFile(guard)
	if !bytes.Equal(persisted, app.guardFile(next.Scope)) {
		t.Fatal("boot scope not saved")
	}
	snapshot, _ = exec.Command("nft", "-j", "list", "table", "inet", "fw4").Output()
	if result := inspectForwardGuard(snapshot, app.lanDevice, next.Scope); result.Status != "pass" {
		t.Fatalf("live scope not updated: %+v", result)
	}
}
