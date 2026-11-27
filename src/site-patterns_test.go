package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
)

func TestDeepSiteWildcardSelection(t *testing.T) {
	for _, p := range []string{"*.a.com", "*.b.a.com", "b.a.com"} {
		if !validSiteDomain(p) {
			t.Fatal("rejected", p)
		}
	}
	for _, p := range []string{"*", "a.*.com", "**.a.com", "*a.com", "*.a..com", "*.*.a.com"} {
		if validSiteDomain(p) {
			t.Fatal("accepted", p)
		}
	}
	broad := testSite(t, "*.a.com")
	narrow := testSite(t, "*.b.a.com")
	exact := testSite(t, "b.a.com")
	app := testApp(t, config{Version: 1, Sites: []siteConfig{broad, narrow, exact}})
	for _, tc := range []struct{ host, key string }{
		{"x.a.com", "*.a.com"}, {"x.y.a.com", "*.a.com"}, {"b.a.com", "b.a.com"},
		{"c.b.a.com", "*.b.a.com"}, {"d.c.b.a.com", "*.b.a.com"}, {"C.B.A.COM.", "*.b.a.com"},
		{"a.com", ""}, {"nota.com", ""}, {"x.a.com.evil", ""}, {".a.com", ""},
	} {
		pair, err := app.getCertificate(&tls.ClientHelloInfo{ServerName: tc.host})
		if tc.key == "" {
			if err == nil {
				t.Fatal("accepted unrelated SNI", tc.host)
			}
			continue
		}
		if err != nil || !bytes.Equal(pair.Certificate[0], app.current.Load().certificates[tc.key].Certificate[0]) {
			t.Fatal("wrong SNI selection", tc.host, err)
		}
	}
	// Configuration matching is deliberately broader than TLS certificate coverage.
	leaf, err := x509.ParseCertificate(app.current.Load().certificates[broad.Domain].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.VerifyHostname("x.a.com") != nil || leaf.VerifyHostname("x.y.a.com") == nil {
		t.Fatal("unexpected certificate wildcard semantics")
	}
	if certificateCoversSite(leaf, "*.other.com") == nil {
		t.Fatal("unrelated certificate accepted")
	}
	for _, name := range []string{"x.a.com", "x.y.a.com", "c.b.a.com"} {
		ip, ok := app.current.Load().dnsRecord(name)
		if !ok || !ip.Equal(app.lanAddr.AsSlice()) {
			t.Fatal("missing wildcard DNS", name)
		}
	}
	if _, ok := app.current.Load().dnsRecord("a.com"); ok {
		t.Fatal("apex matched wildcard")
	}
	app.current.Load().records["x.y.a.com"] = net.ParseIP("203.0.113.9")
	if ip, _ := app.current.Load().dnsRecord("x.y.a.com"); !ip.Equal(net.ParseIP("203.0.113.9")) {
		t.Fatal("explicit DNS lost precedence")
	}
	for _, site := range app.current.Load().sites() {
		host := diagnosticSiteDomain(app.current.Load(), site.Domain)
		if app.current.Load().config.siteKey(host) != site.Domain {
			t.Fatal("diagnostic selected another site")
		}
	}
}

func TestGeneratedWildcardCertificateReuse(t *testing.T) {
	app := testApp(t, config{Version: 1, Sites: []siteConfig{{Domain: "*.a.com"}}})
	before := app.current.Load()
	next, err := app.prepare(before.config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.tls.Certificate[0], next.tls.Certificate[0]) {
		t.Fatal("wildcard certificate regenerated")
	}
}
