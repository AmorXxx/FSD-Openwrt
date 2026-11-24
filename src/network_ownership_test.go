package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLegacyWiFiIgnoredWithoutNetworkWrites(t *testing.T) {
	app := profileTestApp(t)
	current, _ := yaml.Marshal(app.current.Load().config)
	legacy := append([]byte("wifi:\n  upstream: wifi\n  ap_ssid: DoNotApply\n  ap_password: private-legacy-password\n  sta_ssid: OtherNetwork\n"), current...)
	// Any network management command would leave a marker. No STA capability is required.
	bin := t.TempDir()
	marker := filepath.Join(bin, "network-command")
	for _, name := range []string{"uci", "ubus", "wifi", "ip"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho called >> '"+marker+"'\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	cfg, err := app.validateImport(legacy)
	if err != nil {
		t.Fatal(err)
	}
	exported, _ := yaml.Marshal(cfg.publicConfig())
	j, _ := json.Marshal(cfg)
	for _, out := range [][]byte{exported, j} {
		if bytes.Contains(out, []byte("wifi")) || bytes.Contains(out, []byte("private-legacy")) {
			t.Fatal("Wi-Fi leaked into export")
		}
	}
	if err = app.replaceConfig(legacy); err != nil {
		t.Fatal(err)
	}
	active, _ := app.activeProfile()
	saved, _ := os.ReadFile(app.profileFile(active))
	if bytes.Contains(saved, []byte("wifi:")) {
		t.Fatal("legacy Wi-Fi persisted on apply")
	}
	if err = writePrivate(app.profileFile("legacy"), legacy); err != nil {
		t.Fatal(err)
	}
	if err = app.changeProfile(profileAction{Action: "copy", Name: "copy", Source: "legacy", Active: active}); err != nil {
		t.Fatal(err)
	}
	copied, _ := os.ReadFile(app.profileFile("copy"))
	if bytes.Contains(copied, []byte("wifi:")) {
		t.Fatal("legacy Wi-Fi persisted on copy")
	}
	if err = app.changeProfile(profileAction{Action: "switch", Name: "legacy", Active: active}); err != nil {
		t.Fatal(err)
	}
	saved, _ = os.ReadFile(app.profileFile("legacy"))
	if bytes.Contains(saved, []byte("wifi:")) {
		t.Fatal("legacy Wi-Fi persisted on switch")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("FSD invoked network management")
	}
	if _, err = decodeConfig(append(current, []byte("unknown_setting: true\n")...)); err == nil {
		t.Fatal("unrelated unknown fields accepted")
	}
}

func TestObservedUpstream(t *testing.T) {
	if actualUpstream([]byte("default dev sipa_eth8"), nil, "sipa_eth8") != "sim" {
		t.Fatal("SIM route not detected")
	}
	if actualUpstream([]byte("default via 192.0.2.1 dev eth0"), nil, "sipa_eth8") != "openwrt" {
		t.Fatal("incorrect SIM label")
	}
	if actualUpstream(nil, nil, "") != "openwrt" {
		t.Fatal("missing route default")
	}
}
