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
	cfg := config{
		Version: 1,
		Site:    siteConfig{Domain: "site.home.arpa", Certificate: "fixture-cert", Key: "fixture-key"},
		DNS:     []dnsRecord{{Name: "previous.home.arpa", IP: "ap"}},
		Blacklist: dnsBlacklist{
			Block: []string{"blocked.home.arpa"},
			Allow: []string{"allowed.home.arpa"},
		},
		Routes: []customRoute{{Path: "/previous", JSON: `{"\u0061":9007199254740993}`}},
	}
	pair := &tls.Certificate{}
	previous := app.runtimeSettings(cfg, pair)
	app.current.Store(previous)
	input := editorUpdate{
		Revision: snapshotForEditor(cfg).Revision,
		DNS:      []dnsRecord{{Name: "NEW.home.arpa.", IP: ""}},
		Routes: []editorRouteUpdate{
			{Path: "/previous", Source: "/previous"},
			{Path: "/new", JSON: `{"enabled": false}`},
		},
	}
	saved, err := app.saveEditor(input)
	if err != nil {
		t.Fatal(err)
	}
	settings := app.current.Load()
	if saved.Revision == input.Revision || settings.tls != pair ||
		settings.records["new.home.arpa"].String() != "192.0.2.1" ||
		settings.records["previous.home.arpa"] != nil ||
		!slices.Equal(settings.Routes, []customRoute{cfg.Routes[0], {Path: "/new", JSON: `{"enabled":false}`}}) {
		t.Fatal("runtime DNS, routes, revision, or TLS do not match the update")
	}
	file := filepath.Join(app.dir, "config.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := decodeConfig(data)
	if err != nil || persisted.Site != cfg.Site ||
		!slices.Equal(persisted.Blacklist.Block, cfg.Blacklist.Block) ||
		!slices.Equal(persisted.Blacklist.Allow, cfg.Blacklist.Allow) ||
		!slices.Equal(persisted.DNS, settings.DNS) || !slices.Equal(persisted.Routes, settings.Routes) {
		t.Fatal("saved YAML did not preserve the other settings")
	}
	if _, err := app.saveEditor(input); err == nil {
		t.Fatal("stale editor save was accepted")
	}
	bad := editorUpdate{Revision: saved.Revision, DNS: []dnsRecord{}, Routes: []editorRouteUpdate{{Path: "/new", JSON: `{"enabled":}`}}}
	if _, err := app.saveEditor(bad); err == nil || app.current.Load() != settings {
		t.Fatal("invalid JSON changed running settings")
	}
	after, err := os.ReadFile(file)
	if err != nil || !slices.Equal(after, data) {
		t.Fatal("rejected changes altered the active configuration file")
	}
}

func TestEditorPersistentBlacklist(t *testing.T) {
	app := &application{dir: t.TempDir(), lanAddr: netip.MustParseAddr("192.0.2.1"), monitor: newMonitor()}
	cfg := config{Version: 1, Site: siteConfig{Domain: "site.home.arpa"}, Routes: []customRoute{{Path: "/a", JSON: `{"large":9007199254740993}`}}}
	app.current.Store(app.runtimeSettings(cfg, &tls.Certificate{}))
	draft := func() editorUpdate {
		return editorUpdate{Revision: snapshotForEditor(app.current.Load().config).Revision, DNS: []dnsRecord{}, Routes: []editorRouteUpdate{{Path: "/a", Source: "/a"}}}
	}
	input := draft()
	input.Blacklist = &dnsBlacklist{Block: []string{"EXAMPLE.COM."}, Allow: []string{"API.Example.com."}}
	before := app.current.Load()
	saved, err := app.saveEditor(input)
	if err != nil {
		t.Fatal(err)
	}
	current := app.current.Load()
	if saved.Revision == input.Revision || !slices.Equal(saved.Blacklist.Block, []string{"example.com"}) || !slices.Equal(saved.Blacklist.Allow, []string{"api.example.com"}) {
		t.Fatal("blacklist-only update missing or not normalized")
	}
	if len(before.Blacklist.Block) != 0 || input.Blacklist.Block[0] != "EXAMPLE.COM." {
		t.Fatal("save mutated original state or input")
	}
	if !blockedName("child.example.com", current.config, nil) || blockedName("api.example.com", current.config, nil) || !blockedName("api.example.com", current.config, []string{"example.com"}) {
		t.Fatal("rule precedence incorrect")
	}
	raw, err := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := decodeConfig(raw)
	if err != nil || !slices.Equal(persisted.Blacklist.Block, saved.Blacklist.Block) || !slices.Equal(persisted.Blacklist.Allow, saved.Blacklist.Allow) || persisted.Routes[0].JSON != cfg.Routes[0].JSON {
		t.Fatal("rules or JSON not persisted correctly")
	}
	if _, err := app.saveEditor(input); err == nil {
		t.Fatal("stale blacklist revision accepted")
	}
	// Omitted field from an older UI must not remove the persistent lists.
	legacy := draft()
	legacy.DNS = []dnsRecord{{Name: "test.home.arpa", IP: "ap"}}
	if _, err := app.saveEditor(legacy); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(app.current.Load().Blacklist.Block, saved.Blacklist.Block) {
		t.Fatal("older editor erased rules")
	}
	for _, rules := range []dnsBlacklist{
		{Block: []string{"https://example.com"}, Allow: []string{}},
		{Block: []string{}, Allow: []string{"*.example.com"}},
		{Block: []string{}},
		{Block: make([]string, 129), Allow: []string{}},
	} {
		bad := draft()
		bad.Blacklist = &rules
		old := app.current.Load()
		disk, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
		if _, err := app.saveEditor(bad); err == nil {
			t.Fatal("invalid rules accepted")
		}
		after, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
		if app.current.Load() != old || !slices.Equal(disk, after) {
			t.Fatal("rejected rules changed state")
		}
	}
	empty := draft()
	empty.Blacklist = &dnsBlacklist{Block: []string{}, Allow: []string{}}
	if _, err := app.saveEditor(empty); err != nil {
		t.Fatal(err)
	}
	if len(app.current.Load().Blacklist.Block) != 0 || len(app.current.Load().Blacklist.Allow) != 0 {
		t.Fatal("explicit empty lists did not clear rules")
	}
}
