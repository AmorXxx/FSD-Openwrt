package main

import (
	"crypto/tls"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEditorPreservesJSONTokens(t *testing.T) {
	raw := `{"\u0061":true,"large":9007199254740993,"child":{"option":false}}`
	want := []jsonField{
		{Name: "a", Key: `"\u0061"`, Value: "true"},
		{Name: "large", Key: `"large"`, Value: "9007199254740993"},
		{Name: "child", Key: `"child"`, Value: `{"option":false}`},
	}
	if got := objectFields(raw); !slices.Equal(got, want) {
		t.Fatalf("object fields = %#v", got)
	}
	for _, raw := range []string{`[true,false]`, `{"a":1,"\u0061":2}`, `{"\u0000":true}`} {
		if objectFields(raw) != nil {
			t.Fatalf("expected raw JSON editing for %s", raw)
		}
	}
	if objectFields("{}") == nil {
		t.Fatal("empty object should have an editable field list")
	}
}

func TestEditorUpdatesOnlyItsSections(t *testing.T) {
	app := &application{dir: t.TempDir(), lanAddr: netip.MustParseAddr("192.0.2.1"), monitor: newMonitor()}
	cfg := withTestRoutes(config{
		Version: 1,
		Sites:   []siteConfig{{Domain: "site.home.arpa", Certificate: "/fixture-cert", Key: "/fixture-key"}},
		DNS:     []dnsRecord{{Name: "previous.home.arpa", IP: "ap"}},
		Blacklist: dnsBlacklist{
			Block: []string{"blocked.home.arpa"},
			Allow: []string{"allowed.home.arpa"},
		},
	}, []customRoute{{Path: "/previous", JSON: `{"\u0061":9007199254740993}`}})
	rules, _ := cfg.unifiedDNSRules()
	cfg.setDNSRules(rules)
	pair := &tls.Certificate{}
	previous := app.runtimeSettings(cfg, pair)
	app.current.Store(previous)
	input := editorUpdate{
		Revision: snapshotForEditor(cfg).Revision,
		DNSRules: &[]dnsRule{{Name: "NEW.home.arpa.", Action: "answer", IP: "ap"}},
		Routes: []editorRouteUpdate{
			{Path: "/previous", Source: "/previous"},
			{Path: "/new", JSON: `{"enabled": false}`},
		},
	}
	saved, err := app.testSaveEditor(input)
	if err != nil {
		t.Fatal(err)
	}
	settings := app.current.Load()
	if saved.Revision == input.Revision || settings.tls != pair ||
		settings.DNSRules[0].Name != "new.home.arpa" ||
		settings.records["previous.home.arpa"] != nil ||
		!slices.Equal(settings.Sites[0].Routes, []customRoute{cfg.Sites[0].Routes[0], {Path: "/new", JSON: `{"enabled":false}`}}) {
		t.Fatal("runtime DNS, routes, revision, or TLS do not match the update")
	}
	file := filepath.Join(app.dir, "config.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := decodeConfig(data)
	if err != nil || !sameSiteCertificates(persisted.Sites, cfg.Sites) ||
		!slices.Equal(persisted.Blacklist.Block, cfg.Blacklist.Block) ||
		!slices.Equal(persisted.Blacklist.Allow, cfg.Blacklist.Allow) ||
		!slices.Equal(persisted.DNSRules, settings.DNSRules) || !slices.Equal(persisted.Sites[0].Routes, settings.Sites[0].Routes) {
		t.Fatal("saved YAML did not preserve the other settings")
	}
	if _, err := app.testSaveEditor(input); err == nil {
		t.Fatal("stale editor save was accepted")
	}
	bad := editorUpdate{Revision: saved.Revision, DNS: []dnsRecord{}, Routes: []editorRouteUpdate{{Path: "/new", JSON: `{"enabled":}`}}}
	if _, err := app.testSaveEditor(bad); err == nil || app.current.Load() != settings {
		t.Fatal("invalid JSON changed running settings")
	}
	after, err := os.ReadFile(file)
	if err != nil || !slices.Equal(after, data) {
		t.Fatal("rejected changes altered the active configuration file")
	}
}

func TestEditorUnifiedRules(t *testing.T) {
	app := profileTestApp(t)
	cfg := app.current.Load().config
	rules := []dnsRule{{Name: "*.EXAMPLE.COM.", Action: "block"}, {Name: "API.Example.com.", Action: "forward"}}
	input := editorUpdate{Revision: app.editorSnapshot(cfg).Revision, DNSRules: &rules, Routes: []editorRouteUpdate{{Path: "/a", Source: "/a"}}}
	saved, err := app.testSaveEditor(input)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DNSRules[0].Name != "*.example.com" || saved.DNSRules[1].Name != "api.example.com" {
		t.Fatal("not normalized")
	}
	if rules[0].Name != "*.EXAMPLE.COM." {
		t.Fatal("mutated input")
	}
	if !blockedName("x.example.com", app.current.Load().config, nil) || blockedName("api.example.com", app.current.Load().config, nil) {
		t.Fatal("wrong priority")
	}
	data, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	persisted, err := decodeConfig(data)
	if err != nil || !slices.Equal(persisted.DNSRules, saved.DNSRules) {
		t.Fatal(err)
	}
	if _, err = app.testSaveEditor(input); err == nil {
		t.Fatal("stale accepted")
	}
	empty := []dnsRule{}
	input.Revision = saved.Revision
	input.DNSRules = &empty
	if _, err = app.testSaveEditor(input); err != nil || len(app.current.Load().DNSRules) != 0 {
		t.Fatal("clear failed", err)
	}
}
