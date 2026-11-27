package main

import (
	"crypto/tls"
	"github.com/miekg/dns"
	"net"
	"slices"
	"testing"
)

func TestDNSPassthroughValidation(t *testing.T) {
	cfg := scopeTestApp(t).current.Load().config
	cfg.DNSPassthrough = []string{"B.A.COM.", "*.B.A.COM."}
	if err := cfg.validate(); err != nil || !slices.Equal(cfg.DNSPassthrough, []string{"b.a.com", "*.b.a.com"}) {
		t.Fatal(err)
	}
	for _, rules := range [][]string{{"*"}, {"a.*.com"}, {"https://b.a.com"}, {"b.a.com", "B.A.COM."}, make([]string, 129)} {
		cfg.DNSPassthrough = rules
		if cfg.validate() == nil {
			t.Fatal("invalid passthrough accepted", rules)
		}
	}
}
func TestDNSPassthroughForwarding(t *testing.T) {
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		r := new(dns.Msg)
		r.SetReply(q)
		switch q.Question[0].Qtype {
		case dns.TypeA:
			r.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 123}, A: net.ParseIP("203.0.113.7")}}
		case dns.TypeAAAA:
			r.Answer = []dns.RR{&dns.AAAA{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 123}, AAAA: net.ParseIP("2001:db8::7")}}
		case dns.TypeTXT:
			r.Rcode = dns.RcodeNameError
		}
		w.WriteMsg(r)
	})
	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			server := &dns.Server{Handler: handler}
			var address string
			if network == "udp" {
				pc, e := net.ListenPacket("udp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				server.PacketConn = pc
				address = pc.LocalAddr().String()
			} else {
				ln, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				server.Listener = ln
				address = ln.Addr().String()
			}
			ready := make(chan struct{})
			server.NotifyStartedFunc = func() { close(ready) }
			go server.ActivateAndServe()
			<-ready
			defer server.Shutdown()
			app := scopeTestApp(t)
			app.dnsUpstream = address
			cfg := app.current.Load().config
			cfg.Sites = []siteConfig{{Domain: "*.home.arpa"}}
			cfg.DNS = []dnsRecord{{Name: "*.a.com", IP: "192.0.2.1"}, {Name: "b.a.com", IP: "192.0.2.2"}}
			cfg.DNSPassthrough = []string{"b.a.com", "*.deep.a.com", "site.home.arpa"}
			for _, tc := range []struct {
				name                    string
				kind                    uint16
				ip                      string
				block, allow, nontarget bool
				code                    int
			}{
				{name: "b.a.com", kind: dns.TypeA, ip: "203.0.113.7"},
				{name: "b.a.com", kind: dns.TypeAAAA, ip: "2001:db8::7"},
				{name: "b.a.com", kind: dns.TypeTXT, code: dns.RcodeNameError},
				{name: "c.a.com", kind: dns.TypeA, ip: "192.0.2.1"},
				{name: "x.b.a.com", kind: dns.TypeA, ip: "192.0.2.1"},
				{name: "x.y.deep.a.com", kind: dns.TypeA, ip: "203.0.113.7"},
				{name: "deep.a.com", kind: dns.TypeA, ip: "192.0.2.1"},
				{name: "site.home.arpa", kind: dns.TypeA, ip: "203.0.113.7"},
				{name: "b.a.com", kind: dns.TypeA, block: true, code: dns.RcodeNameError},
				{name: "b.a.com", kind: dns.TypeA, block: true, allow: true, ip: "203.0.113.7"},
				{name: "c.a.com", kind: dns.TypeA, nontarget: true, ip: "203.0.113.7"},
			} {
				cfg.Blacklist = dnsBlacklist{}
				if tc.block {
					cfg.Blacklist.Block = []string{"*.a.com"}
				}
				if tc.allow {
					cfg.Blacklist.Allow = []string{"b.a.com"}
				}
				if e := cfg.validate(); e != nil {
					t.Fatal(e)
				}
				app.current.Store(app.runtimeSettings(cfg, &tls.Certificate{}))
				client := "192.0.2.10"
				if tc.nontarget {
					client = "192.0.2.20"
				}
				var remote net.Addr = &net.UDPAddr{IP: net.ParseIP(client), Port: 5000}
				if network == "tcp" {
					remote = &net.TCPAddr{IP: net.ParseIP(client), Port: 5000}
				}
				w := &scopeDNSWriter{remote: remote}
				q := new(dns.Msg)
				q.SetQuestion(tc.name+".", tc.kind)
				app.serveDNS(w, q)
				if w.reply == nil || w.reply.Rcode != tc.code {
					t.Fatal(tc, w.reply)
				}

				// The new unified model must preserve this legacy result over
				// both UDP and TCP, including AAAA, NXDOMAIN and device scope.
				rules, e := cfg.unifiedDNSRules()
				if e != nil {
					t.Fatal(e)
				}
				converted := cfg
				converted.setDNSRules(rules)
				app.current.Store(app.runtimeSettings(converted, &tls.Certificate{}))
				nextWriter := &scopeDNSWriter{remote: remote}
				app.serveDNS(nextWriter, q)
				if nextWriter.reply.Rcode != w.reply.Rcode || len(nextWriter.reply.Answer) != len(w.reply.Answer) {
					t.Fatal("migration response changed", tc)
				}
				for i := range w.reply.Answer {
					if nextWriter.reply.Answer[i].String() != w.reply.Answer[i].String() {
						t.Fatal("migration answer changed", tc)
					}
				}
				if tc.ip != "" {
					if len(w.reply.Answer) != 1 {
						t.Fatal(tc, w.reply)
					}
					var got string
					switch rr := w.reply.Answer[0].(type) {
					case *dns.A:
						got = rr.A.String()
					case *dns.AAAA:
						got = rr.AAAA.String()
					}
					if got != tc.ip {
						t.Fatal(tc, got)
					}
				}
			}
		})
	}
}
