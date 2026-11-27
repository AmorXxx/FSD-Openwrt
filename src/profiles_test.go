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
	cfg := app.current.Load().config
	rules, err := cfg.unifiedDNSRules()
	if err != nil {
		t.Fatal(err)
	}
	cfg.setDNSRules(rules)
	app.current.Store(app.runtimeSettings(cfg, app.current.Load().tls))
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
	act("save", "工作.yaml", "")
	cfg := app.current.Load().config
	cfg.Sites[0].Routes = []customRoute{{Path: "/work", JSON: `{"work":true}`}}
	data, _ := yaml.Marshal(cfg)
	if err := writePrivate(app.profileFile("工作.yaml"), data); err != nil {
		t.Fatal(err)
	}
	act("switch", "工作.yaml", "")
	if app.current.Load().Sites[0].Routes[0].Path != "/work" {
		t.Fatal("runtime not switched")
	}
	if err := app.initProfiles(); err != nil {
		t.Fatal(err)
	}
	loaded, _ := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	persisted, err := decodeConfig(loaded)
	if err != nil || persisted.Sites[0].Routes[0].Path != "/work" {
		t.Fatal("restart lost selection", err)
	}
	cfg.Sites[0].Routes = []customRoute{{Path: "/saved", JSON: `{}`}}
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
	act("rename", "工作新版.yaml", "工作.yaml")
	act("copy", "副本.yaml", "工作新版.yaml")
	if err := app.changeProfile(profileAction{Action: "delete", Name: "工作新版.yaml", Active: "工作新版.yaml"}); err == nil {
		t.Fatal("deleted active")
	}
	act("delete", "副本.yaml", "")
	act("switch", initial, "")
	if app.current.Load().Sites[0].Routes[0].Path != "/a" {
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
	writePrivate(app.profileFile("bad.yaml"), []byte("not: valid"))
	if err := app.changeProfile(profileAction{Action: "switch", Name: "bad.yaml", Active: active}); err == nil {
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
	if err := app.changeProfile(profileAction{Action: "save", Name: "second.yaml", Active: active}); err != nil {
		t.Fatal(err)
	}
	if err := app.changeProfile(profileAction{Action: "switch", Name: "second.yaml", Active: active}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.testSaveEditor(editorUpdate{Revision: previous.Revision}); err == nil {
		t.Fatal("old editor can overwrite new profile")
	}
	external := filepath.Join(t.TempDir(), "outside.yaml")
	os.WriteFile(external, []byte("private"), 0600)
	os.Symlink(external, app.profileFile("link.yaml"))
	if err := app.changeProfile(profileAction{Action: "switch", Name: "link.yaml", Active: "second.yaml"}); err == nil {
		t.Fatal("accepted symlink profile")
	}
}

func TestYAMLOriginalFilenameImportAndApply(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	if active != "config.yaml" {
		t.Fatalf("unexpected migration filename %q", active)
	}
	original, _ := os.ReadFile(app.profileFile(active))
	name := "home.v2 配置.yml"
	data := append([]byte("# original filename and comment\n"), original...)
	if err := app.changeProfile(profileAction{Action: "import", Name: name, Active: active, YAML: string(data)}); err != nil {
		t.Fatal(err)
	}
	imported, _ := os.ReadFile(app.profileFile(name))
	if !bytes.Equal(data, imported) {
		t.Fatal("import changed YAML")
	}
	state, _ := app.profilesState()
	if state.Active != active || len(state.Names) != 2 {
		t.Fatal("import switched active file or omitted yml", state)
	}
	if err := app.changeProfile(profileAction{Action: "import", Name: name, Active: active, YAML: string(original)}); err == nil {
		t.Fatal("overwrote duplicate")
	}
	if err := app.changeProfile(profileAction{Action: "apply", Name: name, Active: active, YAML: string(data)}); err == nil {
		t.Fatal("edited inactive file")
	}
	if err := app.changeProfile(profileAction{Action: "switch", Name: name, Active: active}); err != nil {
		t.Fatal(err)
	}
	if err := app.initProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := app.changeProfile(profileAction{Action: "apply", Name: active, Active: active, YAML: string(original)}); err == nil {
		t.Fatal("stale YAML editor overwrote new file")
	}
	if err := app.changeProfile(profileAction{Action: "apply", Name: name, Active: name, YAML: string(data)}); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(app.profileFile(active))
	if !bytes.Equal(original, unchanged) {
		t.Fatal("apply modified other file")
	}
	for _, bad := range []string{"../bad.yaml", "nested/a.yml", `nested\a.yml`, ".hidden.yaml", "bad.yaml\n", "no-extension", "a.txt"} {
		if validProfile(bad) {
			t.Fatalf("accepted unsafe filename %q", bad)
		}
	}
}

func TestExistingYAMLNamePreserved(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	old := "默认方案.yaml"
	if err := os.Rename(app.profileFile(active), app.profileFile(old)); err != nil {
		t.Fatal(err)
	}
	if err := app.pointProfile(old); err != nil {
		t.Fatal(err)
	}
	if err := app.initProfiles(); err != nil {
		t.Fatal(err)
	}
	state, err := app.profilesState()
	if err != nil || state.Active != old || len(state.Names) != 1 || state.Names[0] != old {
		t.Fatal("legacy filename changed", state, err)
	}
}
