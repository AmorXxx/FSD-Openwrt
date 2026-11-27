package main

import (
	"errors"
	"fmt"
	"net"
)

type dnsRule struct {
	Name   string `yaml:"name" json:"name"`
	Action string `yaml:"action" json:"action"`
	IP     string `yaml:"ip,omitempty" json:"ip,omitempty"`
}

// HTTPS mappings and explicit DNS actions share one specificity order.
// Visit explicit rules first and replace only with a more specific match,
// preserving explicit overrides when the patterns are identical.
func (cfg config) dnsRuleFor(name string) (dnsRule, bool) {
	name = domainName(name)
	var best dnsRule
	found := false
	consider := func(r dnsRule) {
		if !siteDomainMatches(r.Name, name) {
			return
		}
		if !found || (best.Name != name && (r.Name == name || len(r.Name) > len(best.Name))) {
			best, found = r, true
		}
	}
	for _, r := range cfg.DNSRules {
		consider(r)
	}
	for _, site := range cfg.sites() {
		consider(dnsRule{Name: site.Domain, Action: "answer", IP: "ap"})
	}
	return best, found
}
func (cfg *config) validateDNSRules() error {
	if cfg.DNSRules == nil {
		return nil
	}
	if len(cfg.DNS) > 0 || len(cfg.DNSPassthrough) > 0 || len(cfg.Blacklist.Block) > 0 || len(cfg.Blacklist.Allow) > 0 {
		return errors.New("dns_rules 不能与旧 dns、dns_passthrough 或 dns_blacklist 混用")
	}
	if len(cfg.DNSRules) > 1024 {
		return errors.New("DNS 规则最多 1024 条")
	}
	seen := map[string]bool{}
	for i := range cfg.DNSRules {
		r := &cfg.DNSRules[i]
		r.Name = domainName(r.Name)
		if !validSiteDomain(r.Name) || seen[r.Name] {
			return fmt.Errorf("invalid or duplicate DNS rule at index %d", i+1)
		}
		seen[r.Name] = true
		switch r.Action {
		case "answer":
			if r.IP == "" {
				r.IP = "ap"
			}
			if r.IP != "ap" && (net.ParseIP(r.IP) == nil || net.ParseIP(r.IP).To4() == nil) {
				return fmt.Errorf("invalid DNS rule IPv4 at index %d", i+1)
			}
		case "forward", "block":
			if r.IP != "" {
				return fmt.Errorf("DNS rule %d: ip 仅用于 answer", i+1)
			}
		default:
			return fmt.Errorf("invalid DNS rule action at index %d", i+1)
		}
	}
	return nil
}

func (cfg *config) setDNSRules(rules []dnsRule) {
	cfg.DNSRules = append([]dnsRule{}, rules...)
	cfg.DNS = nil
	cfg.DNSPassthrough = nil
	cfg.Blacklist = dnsBlacklist{}
}
