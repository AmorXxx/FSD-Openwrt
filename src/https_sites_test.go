package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func testSite(t *testing.T, domain string) siteConfig {
	t.Helper()
	dir := t.TempDir()
	if _, err := certificate(dir, siteConfig{Domain: domain}, net.ParseIP("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(filepath.Join(dir, "local.crt"))
	key, _ := os.ReadFile(filepath.Join(dir, "local.key"))
	return siteConfig{Domain: domain, Certificate: string(cert), Key: string(key)}
}

func testApp(t *testing.T, cfg config) *application {
	t.Helper()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	app := &application{dir: t.TempDir(), lanAddr: netip.MustParseAddr("127.0.0.1"), monitor: newMonitor()}
	settings, err := app.prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	app.current.Store(settings)
	content, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(app.dir, "config.yaml"), content, 0600); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestSNICertificatesAndReload(t *testing.T) {
	first, second := testSite(t, "one.home.arpa"), testSite(t, "two.home.arpa")
	cfg := config{Version: 1, Sites: []siteConfig{first, second}, Routes: []customRoute{{Path: "/api/demo", JSON: `{"ok":true}`}}}
	app := testApp(t, cfg)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(app.publicSite), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: app.getCertificate}}
	go server.ServeTLS(listener, "", "")
	defer server.Close()
	app.sitePort = listener.Addr().(*net.TCPAddr).Port
	probes := app.protocolProbe(context.Background(), app.current.Load())
	if len(probes) != 4 {
		t.Fatal("missing per-site protocol probes")
	}
	for _, probe := range probes {
		if !probe.OK {
			t.Fatalf("HTTPS probe failed: %+v", probe)
		}
	}
	for _, site := range cfg.sites() {
		if app.current.Load().records[site.Domain] == nil {
			t.Fatal("site DNS missing")
		}
	}
	check := func(domain string, site siteConfig) {
		t.Helper()
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(site.Certificate)) {
			t.Fatal("invalid fixture")
		}
		conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: roots, ServerName: domain})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		expected, _ := customCertificate(site)
		if !bytes.Equal(conn.ConnectionState().PeerCertificates[0].Raw, expected.Certificate[0]) {
			t.Fatal("wrong SNI certificate")
		}
	}
	check(first.Domain, first)
	check(second.Domain, second)
	check("TWO.HOME.ARPA", second)
	if conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, ServerName: "unknown.home.arpa"}); err == nil {
		conn.Close()
		t.Fatal("unknown SNI accepted")
	}
	if pair, err := app.getCertificate(&tls.ClientHelloInfo{}); err != nil || pair != app.current.Load().tls {
		t.Fatal("no-SNI fallback changed")
	}
	// Updating only the second domain must replace that certificate on new connections.
	replacement := testSite(t, second.Domain)
	cfg.Sites[1] = replacement
	body, _ := yaml.Marshal(cfg)
	if err := app.replaceConfig(body); err != nil {
		t.Fatal(err)
	}
	check(first.Domain, first)
	check(second.Domain, replacement)
	// Redacted round trip must retain per-domain secrets, even when list order changes.
	redacted := app.current.Load().publicConfig()
	if redacted.Sites[0].Key != "" || app.current.Load().Sites[0].Key == "" {
		t.Fatal("redaction mutated live config")
	}
	redacted.Sites[0], redacted.Sites[1] = redacted.Sites[1], redacted.Sites[0]
	body, _ = yaml.Marshal(redacted)
	if err := app.replaceConfig(body); err != nil {
		t.Fatal(err)
	}
	check(first.Domain, first)
	check(second.Domain, replacement)
	// Editing routes must retain all SNI certificates.
	snapshot := snapshotForEditor(app.current.Load().config)
	_, err = app.saveEditor(editorUpdate{Revision: snapshot.Revision, DNS: []dnsRecord{}, Routes: []editorRouteUpdate{{Path: "/other", JSON: `{"changed":true}`}}})
	if err != nil {
		t.Fatal(err)
	}
	check(first.Domain, first)
	check(second.Domain, replacement)
	saved, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	persisted, err := decodeConfig(saved)
	if err != nil {
		t.Fatal(err)
	}
	restarted := testApp(t, persisted)
	for _, domain := range []string{first.Domain, second.Domain} {
		if !bytes.Equal(restarted.current.Load().certificates[domain].Certificate[0], app.current.Load().certificates[domain].Certificate[0]) {
			t.Fatal("certificate not persisted")
		}
	}
}

func TestSiteValidationAndLegacy(t *testing.T) {
	old := config{Version: 1, Site: siteConfig{Domain: "legacy.home.arpa"}}
	app := testApp(t, old)
	if app.current.Load().records["legacy.home.arpa"] == nil {
		t.Fatal("legacy DNS missing")
	}
	for _, input := range []string{
		"version: 1\nh2_site: {domain: a.example}\nh2_sites: [{domain: b.example}]\n",
		"version: 1\nh2_sites: [{domain: a.example}, {domain: A.EXAMPLE.}]\n",
		"version: 1\nh2_sites: [{domain: a.example, key: missing-cert}]\n",
		"version: 1\nh2_sites: []\n",
		"version: 1\nh2_sites: [{domain: a.example, unknown: value}]\n",
	} {
		if _, err := decodeConfig([]byte(input)); err == nil {
			t.Fatalf("accepted invalid config %s", input)
		}
	}
	site := testSite(t, "correct.example")
	site.Domain = "wrong.example"
	if _, err := customCertificate(site); err == nil {
		t.Fatal("wrong hostname accepted")
	}
	// Rejected certificate import must not change the active config or stored file.
	before, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	previous := app.current.Load()
	content, _ := yaml.Marshal(config{Version: 1, Sites: []siteConfig{site}})
	if err := app.replaceConfig(content); err == nil {
		t.Fatal("invalid certificate applied")
	}
	after, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	if previous != app.current.Load() || !bytes.Equal(before, after) {
		t.Fatal("failed apply changed state")
	}
}

func TestExactRoutePaths(t *testing.T) {
	cfg := config{Version: 1, Site: siteConfig{Domain: "site.home.arpa"}, Routes: []customRoute{
		{Path: "/api/demo", JSON: `{"match":"plain"}`},
		{Path: "/API/demo", JSON: `{"match":"case"}`},
		{Path: "/api/demo/", JSON: `{"match":"slash"}`},
	}}
	app := testApp(t, cfg)
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/api/demo", `{"match":"plain"}`, 200},
		{"/api/demo?x=1", `{"match":"plain"}`, 200},
		{"/API/demo", `{"match":"case"}`, 200},
		{"/api/demo/", `{"match":"slash"}`, 200},
		{"/prefix/api/demo", "", 404}, {"/api/demo/extra", "", 404}, {"/api/demox", "", 404},
		{"/demo", "", 404}, {"/api/DEMO", "", 404}, {"/api//demo", "", 404},
	} {
		r := httptest.NewRecorder()
		app.publicSite(r, httptest.NewRequest("GET", "https://site.home.arpa"+tc.path, nil))
		body, _ := io.ReadAll(r.Result().Body)
		if r.Code != tc.status || (tc.body != "" && string(body) != tc.body) {
			t.Errorf("%s: %d %s", tc.path, r.Code, body)
		}
	}
	cfg.Routes = append(cfg.Routes, customRoute{Path: "/api/demo", JSON: `{}`})
	if err := cfg.validate(); err == nil {
		t.Fatal("duplicate exact route accepted")
	}
}

func TestGeneratedSiteCertificatesRemainDistinct(t *testing.T) {
	cfg := config{Version: 1, Sites: []siteConfig{{Domain: "first.home.arpa"}, {Domain: "second.home.arpa"}}}
	app := testApp(t, cfg)
	before := app.current.Load()
	if bytes.Equal(before.certificates[cfg.Sites[0].Domain].Certificate[0], before.certificates[cfg.Sites[1].Domain].Certificate[0]) {
		t.Fatal("sites share generated certificate")
	}
	next, err := app.prepare(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range cfg.Sites {
		if !bytes.Equal(before.certificates[site.Domain].Certificate[0], next.certificates[site.Domain].Certificate[0]) {
			t.Fatal("generated certificate changed on reload")
		}
	}
}
