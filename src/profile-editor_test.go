package main

import (
	"bytes"
	"gopkg.in/yaml.v3"
	"os"
	"testing"
)

func TestSelectedFileEditsDoNotApply(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	original := app.current.Load()
	before, _ := os.ReadFile(app.profileFile(active))
	name := "offline.yml"
	if err := app.changeProfile(profileAction{Action: "copy", Name: name, Source: active, Active: active}); err != nil {
		t.Fatal(err)
	}
	doc, _, err := app.readProfileDocument(name)
	if err != nil {
		t.Fatal(err)
	}
	cfg := original.config
	cfg.Routes = []customRoute{{Path: "/offline", JSON: `{"n":9007199254740993}`}}
	data, _ := yaml.Marshal(cfg)
	saved, err := app.editProfileDocument(profileAction{Action: "write_yaml", Name: name, Revision: doc.Revision, YAML: string(data)})
	if err != nil {
		t.Fatal(err)
	}
	if app.current.Load() != original {
		t.Fatal("file save changed runtime")
	}
	current, _ := app.activeProfile()
	if current != active {
		t.Fatal("file save switched selection")
	}
	unchanged, _ := os.ReadFile(app.profileFile(active))
	if !bytes.Equal(before, unchanged) {
		t.Fatal("edited another file")
	}
	if _, err = app.editProfileDocument(profileAction{Action: "write_yaml", Name: name, Revision: doc.Revision, YAML: string(data)}); err == nil {
		t.Fatal("stale overwrite accepted")
	}
	u := editorUpdate{Revision: saved.Revision, DNS: []dnsRecord{}, Routes: []editorRouteUpdate{{Path: "/renamed", Source: "/offline"}}}
	saved, err = app.editProfileDocument(profileAction{Action: "write_editor", Name: name, Revision: saved.Revision, Editor: &u})
	if err != nil {
		t.Fatal(err)
	}
	if app.current.Load() != original {
		t.Fatal("form save changed runtime")
	}
	if err = app.changeProfile(profileAction{Action: "switch", Name: name, Active: active}); err != nil {
		t.Fatal(err)
	}
	if app.current.Load().Routes[0].Path != "/renamed" || app.current.Load().Routes[0].JSON != `{"n":9007199254740993}` {
		t.Fatal("apply did not load selected file")
	}
	// Saving the active file still must not apply; explicitly apply the same filename.
	runtime := app.current.Load()
	u.Revision = saved.Revision
	u.Routes = []editorRouteUpdate{{Path: "/later", Source: "/renamed"}}
	_, err = app.editProfileDocument(profileAction{Action: "write_editor", Name: name, Revision: saved.Revision, Editor: &u})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := app.profilesState()
	if !state.Pending || app.current.Load() != runtime {
		t.Fatal("active-file save applied or pending state missing")
	}
	if err = app.changeProfile(profileAction{Action: "switch", Name: name, Active: name}); err != nil {
		t.Fatal(err)
	}
	state, _ = app.profilesState()
	if state.Pending || app.current.Load().Routes[0].Path != "/later" {
		t.Fatal("same-file apply failed")
	}
}

func TestFileEditorPreservesOwnCertificates(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	cfg := app.current.Load().config
	cfg.Site.Certificate = "/file-specific.crt"
	cfg.Site.Key = "/file-specific.key"
	data, _ := yaml.Marshal(cfg)
	name := "certs.yaml"
	if err := writePrivate(app.profileFile(name), data); err != nil {
		t.Fatal(err)
	}
	doc, _, err := app.readProfileDocument(name)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(doc.YAML), []byte("file-specific")) {
		t.Fatal("export leaked certificates")
	}
	if _, err = app.editProfileDocument(profileAction{Action: "write_yaml", Name: name, Revision: doc.Revision, YAML: doc.YAML}); err != nil {
		t.Fatal(err)
	}
	_, saved, err := app.readProfileDocument(name)
	if err != nil || saved.Site.Certificate != cfg.Site.Certificate || saved.Site.Key != cfg.Site.Key {
		t.Fatal("lost selected file certificates", err)
	}
	now, _ := app.activeProfile()
	if now != active {
		t.Fatal("save switched runtime")
	}
}
