package main

import (
	"crypto/tls"
	"gopkg.in/yaml.v3"
	"os"
	"slices"
	"testing"
)

func TestUnifiedDNSActionsAndPriority(t *testing.T) {
	app := scopeTestApp(t)
	cfg := app.current.Load().config
	cfg.Blacklist = dnsBlacklist{}
	cfg.Sites = []siteConfig{{Domain: "*.home.arpa"}}
	cfg.setDNSRules([]dnsRule{{Name: "*.a.com", Action: "answer", IP: "ap"}, {Name: "b.a.com", Action: "forward"}, {Name: "*.b.a.com", Action: "block"}, {Name: "x.b.a.com", Action: "answer", IP: "203.0.113.3"}, {Name: "site.home.arpa", Action: "forward"}})
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	current := app.runtimeSettings(cfg, &tls.Certificate{})
	for _, tc := range []struct{ name, action, ip string }{{"b.a.com", "forward", ""}, {"c.a.com", "answer", "192.0.2.1"}, {"y.z.b.a.com", "block", ""}, {"x.b.a.com", "answer", "203.0.113.3"}, {"site.home.arpa", "forward", ""}, {"a.com", "", ""}} {
		rule, ok := cfg.dnsRuleFor(tc.name)
		if ok != (tc.action != "") || rule.Action != tc.action {
			t.Fatal(tc, rule)
		}
		ip, ok := current.dnsRecord(tc.name)
		if ok != (tc.ip != "") || ok && ip.String() != tc.ip {
			t.Fatal(tc, ip)
		}
		if blockedName(tc.name, cfg, nil) != (tc.action == "block") {
			t.Fatal("wrong blocking", tc)
		}
	}
	if !blockedName("b.a.com", cfg, []string{"a.com"}) {
		t.Fatal("temporary block bypassed")
	}
	slices.Reverse(cfg.DNSRules)
	if r, _ := cfg.dnsRuleFor("x.b.a.com"); r.IP != "203.0.113.3" {
		t.Fatal("order dependent")
	}
}
func TestUnifiedDNSValidationAndEmptyRoundTrip(t *testing.T) {
	cfg := scopeTestApp(t).current.Load().config
	for _, rules := range [][]dnsRule{
		{{Name: "a.com", Action: "invalid"}}, {{Name: "a.com", Action: "forward", IP: "192.0.2.1"}}, {{Name: "a.com", Action: "answer", IP: "bad"}}, {{Name: "a.com", Action: "block"}, {Name: "A.COM.", Action: "forward"}}, {{Name: "a.*.com", Action: "block"}},
	} {
		cfg.setDNSRules(rules)
		if cfg.validate() == nil {
			t.Fatal("accepted invalid rules", rules)
		}
	}
	cfg.setDNSRules([]dnsRule{})
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeConfig(data)
	if err != nil || decoded.DNSRules == nil {
		t.Fatal("empty rules lost", err, string(data))
	}
	cfg.DNS = []dnsRecord{{Name: "a.com", IP: "ap"}}
	if cfg.validate() == nil {
		t.Fatal("mixed formats accepted")
	}
}
func TestUnifiedDNSLegacyMigration(t *testing.T) {
	app := scopeTestApp(t)
	cfg := app.current.Load().config
	cfg.DNS = []dnsRecord{{Name: "*.a.com", IP: "ap"}, {Name: "x.b.a.com", IP: "203.0.113.3"}}
	cfg.DNSPassthrough = []string{"*.p.a.com"}
	cfg.Blacklist = dnsBlacklist{Block: []string{"a.com", "*.p.a.com"}, Allow: []string{"b.a.com", "x.b.a.com", "site.home.arpa"}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	rules, err := cfg.unifiedDNSRules()
	if err != nil {
		t.Fatal(err)
	}
	old := app.runtimeSettings(cfg, &tls.Certificate{})
	converted := cfg
	converted.setDNSRules(rules)
	if err = converted.validate(); err != nil {
		t.Fatal(err)
	}
	next := app.runtimeSettings(converted, &tls.Certificate{})
	for _, name := range []string{"a.com", "b.a.com", "x.b.a.com", "y.b.a.com", "x.p.a.com", "deep.x.p.a.com", "p.a.com", "site.home.arpa", "other.com"} {
		blocked := blockedName(name, cfg, nil)
		if blocked != blockedName(name, converted, nil) {
			t.Fatal("block changed", name)
		}
		if !blocked {
			a, aok := old.dnsRecord(name)
			b, bok := next.dnsRecord(name)
			if aok != bok || aok && !a.Equal(b) {
				t.Fatal("answer changed", name, a, b)
			}
		}
	}
	cfg.Blacklist.Block = []string{"a?.com"}
	if _, err = cfg.unifiedDNSRules(); err == nil {
		t.Fatal("lossy glob migration accepted")
	}
}
func TestUnifiedDNSProfileConversion(t *testing.T) {
	app := profileTestApp(t)
	original := app.current.Load()
	active, _ := app.activeProfile()
	doc, _, _ := app.readProfileDocument(active)
	rules := []dnsRule{{Name: "*.a.com", Action: "block"}, {Name: "b.a.com", Action: "forward"}}
	input := editorUpdate{Revision: doc.Revision, DNSRules: &rules, Routes: []editorRouteUpdate{}}
	saved, err := app.testEditProfileDocument(profileAction{Action: "write_editor", Name: active, Revision: doc.Revision, Editor: &input})
	if err != nil {
		t.Fatal(err)
	}
	if app.current.Load() != original {
		t.Fatal("premature apply")
	}
	data, _ := os.ReadFile(app.profileFile(active))
	cfg, err := decodeConfig(data)
	if err != nil || !slices.Equal(cfg.DNSRules, rules) || len(cfg.DNS) > 0 || len(cfg.Blacklist.Block) > 0 {
		t.Fatal("conversion failed", err)
	}
	if err = app.changeProfile(profileAction{Action: "switch", Name: active, Active: active}); err != nil {
		t.Fatal(err)
	}
	if !blockedName("c.a.com", app.current.Load().config, nil) || blockedName("b.a.com", app.current.Load().config, nil) {
		t.Fatal("applied wrong rules")
	}
	empty := []dnsRule{}
	input.DNSRules = &empty
	input.Revision = saved.Revision
	if _, err = app.testEditProfileDocument(profileAction{Action: "write_editor", Name: active, Revision: saved.Revision, Editor: &input}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(app.profileFile(active))
	cfg, err = decodeConfig(data)
	if err != nil || cfg.DNSRules == nil || len(cfg.DNSRules) != 0 {
		t.Fatal("clear failed", err)
	}
}

func TestUnifiedDNSMigrationKeepsSpecificAutomaticSite(t *testing.T) {
	app := scopeTestApp(t)
	cfg := app.current.Load().config
	cfg.Sites[0].Domain = "b.a.com"
	cfg.Blacklist = dnsBlacklist{}
	cfg.DNS = []dnsRecord{{Name: "*.a.com", IP: "203.0.113.9"}}
	rules, err := cfg.unifiedDNSRules()
	if err != nil {
		t.Fatal(err)
	}
	cfg.setDNSRules(rules)
	current := app.runtimeSettings(cfg, &tls.Certificate{})
	if ip, _ := current.dnsRecord("b.a.com"); ip.String() != "192.0.2.1" {
		t.Fatal("automatic site changed", ip)
	}
	if ip, _ := current.dnsRecord("c.a.com"); ip.String() != "203.0.113.9" {
		t.Fatal("wildcard changed", ip)
	}
}

func TestRemovedYAMLFieldsRejected(t *testing.T) {
	for _, line := range []string{"dns: []", "dns_blacklist: {}", "dns_passthrough: []", "dns: null", "<<: {dns: [{name: a.com, ip: ap}]}"} {
		data := []byte("version: 1\nh2_sites: [{domain: site.home.arpa}]\ndns_rules: []\n" + line + "\n")
		if _, err := decodeConfig(data); err == nil {
			t.Fatal("accepted removed YAML field", line)
		}
	}
}
