package main

import (
	"crypto/tls"
	"github.com/miekg/dns"
	"net"
	"slices"
	"testing"
)

func TestDNSAndSiteSharedPriority(t *testing.T) {
	for _, tc := range []struct{ site, rule, action, host, want string }{
		{"api.a.com", "*.a.com", "block", "api.a.com", "answer"},
		{"api.a.com", "*.a.com", "forward", "api.a.com", "answer"},
		{"*.b.a.com", "*.a.com", "block", "deep.x.b.a.com", "answer"},
		{"*.a.com", "*.b.a.com", "block", "deep.x.b.a.com", "block"},
		{"*.a.com", "api.a.com", "forward", "api.a.com", "forward"},
		{"api.a.com", "api.a.com", "block", "api.a.com", "block"},
		{"api.a.com", "api.a.com", "forward", "api.a.com", "forward"},
		{"*.a.com", "*.a.com", "block", "deep.x.a.com", "block"},
		{"*.a.com", "*.a.com", "forward", "deep.x.a.com", "forward"},
		{"api.a.com", "api.a.com", "answer", "api.a.com", "answer"},
	} {
		t.Run(tc.site+"/"+tc.rule+"/"+tc.action, func(t *testing.T) {
			app := scopeTestApp(t)
			cfg := app.current.Load().config
			cfg.Sites = []siteConfig{{Domain: tc.site}, {Domain: "unused.example"}}
			r := dnsRule{Name: tc.rule, Action: tc.action}
			if r.Action == "answer" {
				r.IP = "203.0.113.7"
			}
			cfg.setDNSRules([]dnsRule{r, {Name: "unrelated.example", Action: "block"}})
			if tc.site == tc.rule {
				if cfg.validate() == nil {
					t.Fatal("same-pattern conflict accepted")
				}
				return
			}
			for i := 0; i < 2; i++ {
				current := app.runtimeSettings(cfg, &tls.Certificate{})
				rule, ok := cfg.dnsRuleFor(tc.host)
				if !ok || rule.Action != tc.want {
					t.Fatal(rule)
				}
				if blockedName(tc.host, cfg, nil) != (tc.want == "block") {
					t.Fatal("firewall blocking disagrees")
				}
				if cfg.passthroughDNS(tc.host) != (tc.want == "forward") {
					t.Fatal("forwarding disagrees")
				}
				ip, ok := current.dnsRecord(tc.host)
				if ok != (tc.want == "answer") {
					t.Fatal("DNS mapping disagrees")
				}
				if ok {
					want := "192.0.2.1"
					if tc.action == "answer" {
						want = "203.0.113.7"
					}
					if ip.String() != want {
						t.Fatal(ip)
					}
				}
				if !blockedName(tc.host, cfg, []string{"a.com"}) {
					t.Fatal("temporary blacklist bypassed")
				}
				slices.Reverse(cfg.DNSRules)
				slices.Reverse(cfg.Sites)
			}
		})
	}
}
func TestExactSiteOverridesWildcardBlockOnDNSWire(t *testing.T) {
	app := scopeTestApp(t)
	cfg := app.current.Load().config
	cfg.Sites = []siteConfig{{Domain: "api.region.example.test"}}
	cfg.setDNSRules([]dnsRule{{Name: "*.example.test", Action: "block"}})
	app.current.Store(app.runtimeSettings(cfg, &tls.Certificate{}))
	for _, network := range []string{"udp", "tcp"} {
		for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
			var addr net.Addr = &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 3000}
			if network == "tcp" {
				addr = &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 3000}
			}
			w := &scopeDNSWriter{remote: addr}
			q := new(dns.Msg)
			q.SetQuestion("api.region.example.test.", qtype)
			app.serveDNS(w, q)
			if w.reply.Rcode != dns.RcodeSuccess {
				t.Fatal("site blocked", w.reply)
			}
			if qtype == dns.TypeA && (len(w.reply.Answer) != 1 || w.reply.Answer[0].(*dns.A).A.String() != "192.0.2.1") {
				t.Fatal("wrong local IP")
			}
			if qtype == dns.TypeAAAA && len(w.reply.Answer) != 0 {
				t.Fatal("unexpected IPv6 mapping")
			}
		}
	}
}
