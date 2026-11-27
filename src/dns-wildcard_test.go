package main

import (
	"crypto/tls"
	"github.com/miekg/dns"
	"net"
	"testing"
)

func TestDNSWildcardValidationAndPrecedence(t *testing.T) {
	app := scopeTestApp(t)
	cfg := app.current.Load().config
	cfg.DNS = []dnsRecord{{Name: "*.A.COM.", IP: "ap"}, {Name: "*.b.a.com", IP: "192.0.2.20"}, {Name: "c.b.a.com", IP: "192.0.2.30"}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	current := app.runtimeSettings(cfg, &tls.Certificate{})
	for _, tc := range []struct{ name, want string }{
		{"b.a.com", "192.0.2.1"}, {"x.y.a.com", "192.0.2.1"},
		{"x.b.a.com", "192.0.2.20"}, {"d.c.b.a.com", "192.0.2.20"},
		{"C.B.A.COM.", "192.0.2.30"},
		{"a.com", ""}, {"nota.com", ""}, {"a.com.evil", ""}, {"x.a.com.evil", ""}, {".a.com", ""},
	} {
		ip, ok := current.dnsRecord(tc.name)
		if tc.want == "" {
			if ok {
				t.Fatal("false match", tc.name)
			}
			continue
		}
		if !ok || ip.String() != tc.want {
			t.Fatal(tc.name, ip, ok)
		}
	}
	for _, bad := range []string{"*", "a.*.com", "**.a.com", "*a.com", "*.*.a.com", "https://a.com", "*.a..com"} {
		cfg.DNS = []dnsRecord{{Name: bad, IP: "ap"}}
		if cfg.validate() == nil {
			t.Fatal("accepted invalid wildcard", bad)
		}
	}
	cfg.DNS = []dnsRecord{{Name: "*.a.com", IP: "ap"}, {Name: "*.A.COM.", IP: "ap"}}
	if cfg.validate() == nil {
		t.Fatal("accepted duplicate wildcard")
	}
}

func TestDNSWildcardAnswers(t *testing.T) {
	app := scopeTestApp(t)
	cfg := app.current.Load().config
	cfg.DNS = []dnsRecord{{Name: "*.a.com", IP: "192.0.2.55"}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	app.current.Store(app.runtimeSettings(cfg, &tls.Certificate{}))
	for _, remote := range []net.Addr{&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5300}, &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5300}} {
		for _, name := range []string{"b.a.com.", "c.b.a.com."} {
			w := &scopeDNSWriter{remote: remote}
			q := new(dns.Msg)
			q.SetQuestion(name, dns.TypeA)
			app.serveDNS(w, q)
			if w.reply == nil || len(w.reply.Answer) != 1 {
				t.Fatal(w.reply)
			}
			a, ok := w.reply.Answer[0].(*dns.A)
			if !ok || a.A.String() != "192.0.2.55" || a.Hdr.Name != name {
				t.Fatal(w.reply)
			}
		}
	}
}
