package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestSitesEditorSaveValidateAndApply(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	doc, _, err := app.readProfileDocument(active)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Editor.Sites) != 1 || doc.Editor.Sites[0].Domain != "site.home.arpa" {
		t.Fatal("sites missing from snapshot")
	}
	original := app.current.Load()
	sites := []siteConfig{testSite(t, "one.home.arpa"), testSite(t, "two.home.arpa")}
	rules := []dnsRule{}
	u := editorUpdate{Revision: doc.Revision, DNSRules: &rules, Sites: editorSitesForTest(sites), Routes: []editorRouteUpdate{{Path: "/a", Source: "/a"}}}
	saved, err := app.testEditProfileDocument(profileAction{Action: "write_editor", Name: active, Revision: doc.Revision, Editor: &u})
	if err != nil {
		t.Fatal(err)
	}
	if app.current.Load() != original {
		t.Fatal("saving applied configuration")
	}
	if len(saved.Editor.Sites) != 2 || saved.Editor.Sites[1].Certificate != sites[1].Certificate || saved.Editor.Sites[1].Domain != sites[1].Domain {
		t.Fatal("site paths lost")
	}
	encoded, _ := json.Marshal(saved.Editor)
	if bytes.Contains(encoded, []byte("BEGIN PRIVATE KEY")) {
		t.Fatal("private key content exposed")
	}
	if err = app.changeProfile(profileAction{Action: "switch", Name: active, Active: active}); err != nil {
		t.Fatal(err)
	}
	if len(app.current.Load().certificates) != 2 || app.current.Load().records["two.home.arpa"] == nil {
		t.Fatal("TLS/DNS not updated")
	}
	doc, _, _ = app.readProfileDocument(active)
	before, _ := os.ReadFile(app.profileFile(active))
	for _, bad := range [][]siteConfig{{}, {sites[0], sites[0]}, {{Domain: "one.home.arpa", Certificate: "/missing.pem", Key: "/missing.key"}}, {{Domain: "one.home.arpa", Certificate: sites[0].Certificate}}} {
		u.Sites = editorSitesForTest(bad)
		u.Revision = doc.Revision
		if _, err = app.testEditProfileDocument(profileAction{Action: "write_editor", Name: active, Revision: doc.Revision, Editor: &u}); err == nil {
			t.Fatal("invalid site accepted")
		}
		after, _ := os.ReadFile(app.profileFile(active))
		if !bytes.Equal(before, after) {
			t.Fatal("failed save changed file")
		}
	}
	// Live editor endpoint must reload certificates instead of reusing the old map.
	one := []siteConfig{sites[1]}
	u.Sites = editorSitesForTest(one)
	u.Revision = app.editorSnapshot(app.current.Load().config).Revision
	if _, err = app.testSaveEditor(u); err != nil {
		t.Fatal(err)
	}
	if len(app.current.Load().certificates) != 1 || app.current.Load().certificates["two.home.arpa"] == nil {
		t.Fatal("live editor reused certificates")
	}
}
