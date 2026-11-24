package main

import (
	"bytes"
	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"testing"
)

func profileTestApp(t *testing.T) *application {
	t.Helper()
	app := scopeTestApp(t)
	data, _ := yaml.Marshal(app.current.Load().config)
	if err := writePrivate(filepath.Join(app.dir, "config.yaml"), data); err != nil {
		t.Fatal(err)
	}
	if err := app.initProfiles(); err != nil {
		t.Fatal(err)
	}
	return app
}
func TestProfileLifecycleAndRestart(t *testing.T) {
	app := profileTestApp(t)
	initial, _ := app.activeProfile()
	act := func(action, name, source string) {
		t.Helper()
		active, _ := app.activeProfile()
		if err := app.changeProfile(profileAction{Action: action, Name: name, Source: source, Active: active}); err != nil {
			t.Fatal(err)
		}
	}
	original, _ := os.ReadFile(app.profileFile(initial))
	act("save", "工作", "")
	cfg := app.current.Load().config
	cfg.Routes = []customRoute{{Path: "/work", JSON: `{"work":true}`}}
	data, _ := yaml.Marshal(cfg)
	if err := writePrivate(app.profileFile("工作"), data); err != nil {
		t.Fatal(err)
	}
	act("switch", "工作", "")
	if app.current.Load().Routes[0].Path != "/work" {
		t.Fatal("runtime not switched")
	}
	if err := app.initProfiles(); err != nil {
		t.Fatal(err)
	}
	loaded, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	persisted, err := decodeConfig(loaded)
	if err != nil || persisted.Routes[0].Path != "/work" {
		t.Fatal("restart lost selection", err)
	}
	cfg.Routes = []customRoute{{Path: "/saved", JSON: `{}`}}
	if err := app.commitSettings(app.runtimeSettings(cfg, app.current.Load().tls), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(app.dir, "config.yaml")); err != nil {
		t.Fatal("save replaced selection link")
	}
	unchanged, _ := os.ReadFile(app.profileFile(initial))
	if !bytes.Equal(original, unchanged) {
		t.Fatal("save changed inactive profile")
	}
	act("rename", "工作新版", "工作")
	act("copy", "副本", "工作新版")
	if err := app.changeProfile(profileAction{Action: "delete", Name: "工作新版", Active: "工作新版"}); err == nil {
		t.Fatal("deleted active")
	}
	act("delete", "副本", "")
	act("switch", initial, "")
	if app.current.Load().Routes[0].Path != "/a" {
		t.Fatal("original profile lost")
	}
}
func TestProfileRejectsInvalidAndRollsBack(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	previous := app.current.Load()
	for _, name := range []string{"../outside", "a/b", "", ".", "a\n"} {
		if err := app.changeProfile(profileAction{Action: "save", Name: name, Active: active}); err == nil {
			t.Fatal("accepted invalid name", name)
		}
	}
	if err := app.changeProfile(profileAction{Action: "save", Name: "x", Active: "stale"}); err == nil {
		t.Fatal("accepted stale state")
	}
	writePrivate(app.profileFile("bad"), []byte("not: valid"))
	if err := app.changeProfile(profileAction{Action: "switch", Name: "bad", Active: active}); err == nil {
		t.Fatal("accepted invalid YAML")
	}
	now, _ := app.activeProfile()
	if now != active || app.current.Load() != previous {
		t.Fatal("invalid switch mutated state")
	}
	// Atomic config commits preserve disk contents when an apply callback fails.
	before, _ := os.ReadFile(app.profileFile(active))
	if err := app.commitSettings(previous, func() error { return errors.New("injected") }); err == nil {
		t.Fatal("expected failure")
	}
	after, _ := os.ReadFile(app.profileFile(active))
	if !bytes.Equal(before, after) {
		t.Fatal("rollback changed profile")
	}
}

func TestProfilePermissionsAndEditorIsolation(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	info, err := os.Stat(app.profileFile(active))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private profile permissions", err)
	}
	previous := app.editorSnapshot(app.current.Load().config)
	if err := app.changeProfile(profileAction{Action: "save", Name: "second", Active: active}); err != nil {
		t.Fatal(err)
	}
	if err := app.changeProfile(profileAction{Action: "switch", Name: "second", Active: active}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.saveEditor(editorUpdate{Revision: previous.Revision}); err == nil {
		t.Fatal("old editor can overwrite new profile")
	}
	external := filepath.Join(t.TempDir(), "outside.yaml")
	os.WriteFile(external, []byte("private"), 0600)
	os.Symlink(external, app.profileFile("link"))
	if err := app.changeProfile(profileAction{Action: "switch", Name: "link", Active: "second"}); err == nil {
		t.Fatal("accepted symlink profile")
	}
}
