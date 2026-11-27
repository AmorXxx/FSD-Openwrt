package main

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Keep legacy files running unchanged. The editor converts hierarchical rules
// at every matching boundary, preserving the old blacklist-first semantics.
// Arbitrary globs cannot be represented by the new suffix-only rule language.
func (cfg config) unifiedDNSRules() ([]dnsRule, error) {
	if cfg.DNSRules != nil {
		return append([]dnsRule{}, cfg.DNSRules...), nil
	}
	candidates := map[string]bool{}
	for _, r := range cfg.DNS {
		candidates[r.Name] = true
	}
	for _, r := range cfg.DNSPassthrough {
		candidates[r] = true
	}
	for _, r := range cfg.Blacklist.Allow {
		candidates[r] = true
	}
	for _, r := range cfg.Blacklist.Block {
		if !validSiteDomain(r) {
			return nil, fmt.Errorf("旧黑名单 %q 使用了无法自动转换的通配符，请在高级 YAML 中手动改为 dns_rules", r)
		}
		candidates[r] = true
		if !strings.HasPrefix(r, "*.") {
			candidates["*."+r] = true
		}
	}
	// Site mappings participated in legacy specificity ordering. Include their
	// boundaries so a broad explicit rule cannot change a more-specific site.
	if len(candidates) > 0 {
		for _, site := range cfg.sites() {
			candidates[site.Domain] = true
		}
	}
	patterns := make([]string, 0, len(candidates))
	for p := range candidates {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	result := make([]dnsRule, 0, len(patterns))
	for _, pattern := range patterns {
		host := pattern
		if strings.HasPrefix(pattern, "*.") {
			for i := 0; ; i++ {
				host = fmt.Sprintf("fsd-migrate-%d.%s", i, pattern[2:])
				collision := false
				for _, p := range patterns {
					if p != pattern && len(p) >= len(pattern) && siteDomainMatches(p, host) {
						collision = true
						break
					}
				}
				if !collision {
					break
				}
			}
		}
		r := dnsRule{Name: pattern, Action: "forward"}
		if blockedName(host, cfg, nil) {
			r.Action = "block"
		} else if !cfg.passthroughDNS(host) {
			best := ""
			ip := ""
			// Auto site records are fallback records; explicit DNS wins ties.
			for _, site := range cfg.sites() {
				if siteDomainMatches(site.Domain, host) && (best == "" || site.Domain == host || len(site.Domain) > len(best) && best != host) {
					best = site.Domain
					ip = "ap"
				}
			}
			for _, record := range cfg.DNS {
				if siteDomainMatches(record.Name, host) && (best == "" || record.Name == host || len(record.Name) >= len(best) && best != host) {
					best = record.Name
					ip = record.IP
				}
			}
			if best != "" {
				r.Action = "answer"
				r.IP = ip
			}
		}
		redundant := false
		for _, site := range cfg.sites() {
			if site.Domain == r.Name {
				if r.Action != "answer" || r.IP != "ap" {
					return nil, fmt.Errorf("域名配置冲突：%s", r.Name)
				}
				redundant = true
			}
		}
		if !redundant {
			result = append(result, r)
		}
	}
	return result, nil
}

func TestDeploymentConvertProfiles(t *testing.T) {
	dir := os.Getenv("FSD_CONVERT_PROFILES")
	if dir == "" {
		t.Skip("deployment-only converter")
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "original", "*.yaml"))
	if len(paths) == 0 {
		t.Fatal("no input profiles")
	}
	os.MkdirAll(filepath.Join(dir, "converted"), 0700)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var cfg config
		if err = yaml.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		if err = cfg.validate(); err != nil {
			t.Fatal(err)
		}
		rules, err := cfg.unifiedDNSRules()
		if err != nil {
			t.Fatal(err)
		}
		cfg.setDNSRules(rules)
		data, err = yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodeConfig(data); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "converted", filepath.Base(p)), data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Log(filepath.Base(p), len(rules), "rules converted")
	}
}
