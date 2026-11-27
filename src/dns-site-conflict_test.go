package main

import (
	"bytes"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
	"testing"
)

func TestDNSSiteConflictValidation(t *testing.T) {
	for _, domain := range []string{"SITE.HOME.ARPA.", "*.HOME.ARPA."} {
		for _, action := range []string{"answer", "forward", "block"} {
			cfg := scopeTestApp(t).current.Load().config
			cfg.Sites = []siteConfig{{Domain: domain}}
			rule := dnsRule{Name: strings.ToLower(domain), Action: action}
			if action == "answer" {
				rule.IP = "ap"
			}
			cfg.setDNSRules([]dnsRule{rule})
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), "域名配置冲突") || !strings.Contains(err.Error(), domainName(domain)) {
				t.Fatalf("missing conflict: %v", err)
			}
		}
	}
}

func TestConflictingProfileSaveLeavesFileAndRuntime(t *testing.T) {
	app := profileTestApp(t)
	active, _ := app.activeProfile()
	doc, _, err := app.readProfileDocument(active)
	if err != nil {
		t.Fatal(err)
	}
	original := app.current.Load()
	path := app.profileFile(active)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rules := []dnsRule{{Name: original.Sites[0].Domain, Action: "block"}}
	update := editorUpdate{Revision: doc.Revision, DNSRules: &rules, Routes: []editorRouteUpdate{}}
	if _, err = app.testEditProfileDocument(profileAction{Action: "write_editor", Name: active, Revision: doc.Revision, Editor: &update}); err == nil {
		t.Fatal("editor conflict accepted")
	}
	cfg := original.config
	cfg.setDNSRules(rules)
	raw, _ := yaml.Marshal(cfg)
	if _, err = app.testEditProfileDocument(profileAction{Action: "write_yaml", Name: active, Revision: doc.Revision, YAML: string(raw)}); err == nil {
		t.Fatal("YAML conflict accepted")
	}
	if err = app.replaceConfig(raw); err == nil {
		t.Fatal("apply conflict accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || app.current.Load() != original {
		t.Fatal("failed validation modified state")
	}
}
