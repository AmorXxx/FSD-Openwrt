package main

import (
	"bytes"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"testing"
)

func TestCertificateFileOnlyConfiguration(t *testing.T) {
	site := testSite(t, "files.example")
	cfg := config{Version: 1, Sites: []siteConfig{site}}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	app := testApp(t, cfg)
	if _, err = app.validateImport(data); err != nil {
		t.Fatal(err)
	}
	exported, _ := yaml.Marshal(cfg.publicConfig())
	if !bytes.Contains(exported, []byte(site.Certificate)) || !bytes.Contains(exported, []byte(site.Key)) || bytes.Contains(exported, []byte("BEGIN PRIVATE KEY")) {
		t.Fatal("incorrect exported file references")
	}
	certPEM, _ := os.ReadFile(site.Certificate)
	keyPEM, _ := os.ReadFile(site.Key)
	for _, bad := range []siteConfig{
		{Domain: site.Domain, Certificate: string(certPEM), Key: string(keyPEM)},
		{Domain: site.Domain, Certificate: "relative.crt", Key: "relative.key"},
		{Domain: site.Domain, Certificate: "https://files.example/cert", Key: site.Key},
		{Domain: site.Domain, Certificate: site.Certificate},
	} {
		badCfg := cfg
		badCfg.Sites[0] = bad
		data, _ = yaml.Marshal(badCfg)
		if _, err = decodeConfig(data); err == nil {
			t.Fatal("accepted non-file or incomplete configuration")
		}
	}
	previous := app.current.Load()
	for _, bad := range []siteConfig{
		{Domain: site.Domain, Certificate: filepath.Join(t.TempDir(), "missing.crt"), Key: site.Key},
		{Domain: site.Domain, Certificate: t.TempDir(), Key: site.Key},
		{Domain: site.Domain, Certificate: site.Certificate, Key: testSite(t, site.Domain).Key},
	} {
		badCfg := cfg
		badCfg.Sites[0] = bad
		data, _ = yaml.Marshal(badCfg)
		if err = app.replaceConfig(data); err == nil {
			t.Fatal("accepted missing/directory/mismatched file")
		}
		if app.current.Load() != previous {
			t.Fatal("invalid certificate changed runtime")
		}
	}
	// Explicitly omitting both paths now removes the custom file references.
	cfg.Sites[0] = siteConfig{Domain: site.Domain}
	data, _ = yaml.Marshal(cfg)
	if err = app.replaceConfig(data); err != nil {
		t.Fatal(err)
	}
	if app.current.Load().Sites[0].Certificate != "" || app.current.Load().Sites[0].Key != "" {
		t.Fatal("restored omitted paths")
	}
	if _, err = os.Stat(site.Key); err != nil {
		t.Fatal("removed original key", err)
	}
}
