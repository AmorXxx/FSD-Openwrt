package main

import (
	"crypto/x509"
	"fmt"
	"net"
	"strings"
)

func validSiteDomain(pattern string) bool {
	if strings.HasPrefix(pattern, "*.") {
		if len(pattern) > 253 {
			return false
		}
		return validDomain(strings.TrimPrefix(pattern, "*."), false)
	}
	return validDomain(pattern, false)
}

// The FSD rule matches any nonempty subdomain depth, not the apex itself.
func siteDomainMatches(pattern, host string) bool {
	host = domainName(host)
	if !validDomain(host, false) {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		return strings.HasSuffix(host, pattern[1:]) && host != pattern[2:]
	}
	return host == pattern
}

func (cfg config) siteKey(host string) string {
	host = domainName(host)
	best := ""
	for _, site := range cfg.sites() {
		if !siteDomainMatches(site.Domain, host) {
			continue
		}
		if site.Domain == host {
			return site.Domain
		}
		if len(site.Domain) > len(best) {
			best = site.Domain
		}
	}
	return best
}

// Explicit records and automatic HTTPS site records share precedence:
// exact name first, then the longest matching wildcard suffix.
func (cfg config) passthroughDNS(name string) bool {
	if cfg.DNSRules != nil {
		r, ok := cfg.dnsRuleFor(name)
		return ok && r.Action == "forward"
	}
	for _, pattern := range cfg.DNSPassthrough {
		if siteDomainMatches(pattern, name) {
			return true
		}
	}
	return false
}

func (current *runtimeConfig) dnsRecord(name string) (net.IP, bool) {
	if current.DNSRules != nil {
		if r, ok := current.dnsRuleFor(name); ok {
			if r.Action != "answer" {
				return nil, false
			}
			if r.IP == "ap" {
				return current.ruleAP, true
			}
			return net.ParseIP(r.IP).To4(), true
		}
	}
	if current.passthroughDNS(name) {
		return nil, false
	}
	name = domainName(name)
	if !validDomain(name, false) {
		return nil, false
	}
	if ip, ok := current.records[name]; ok {
		return ip, true
	}
	best := ""
	for pattern := range current.records {
		if strings.HasPrefix(pattern, "*.") && len(pattern) > len(best) && siteDomainMatches(pattern, name) {
			best = pattern
		}
	}
	if best == "" {
		return nil, false
	}
	return current.records[best], true
}

// Certificate wildcards retain TLS semantics. A wildcard site must supply
// the corresponding wildcard SAN; deeper hosts additionally need coverage.
func certificateCoversSite(leaf *x509.Certificate, pattern string) error {
	if !strings.HasPrefix(pattern, "*.") {
		return leaf.VerifyHostname(pattern)
	}
	for _, san := range leaf.DNSNames {
		if domainName(san) == pattern {
			return nil
		}
	}
	return fmt.Errorf("certificate SAN must include %s; deeper subdomains require additional SAN coverage", pattern)
}

func diagnosticSiteDomain(current *runtimeConfig, pattern string) string {
	if !strings.HasPrefix(pattern, "*.") {
		return pattern
	}
	for n := 0; n < 26; n++ {
		host := fmt.Sprintf("%c.%s", rune('a'+n), pattern[2:])
		if current.config.siteKey(host) == pattern {
			return host
		}
	}
	return "" // Invalid/unvalidated configurations cannot loop forever.

}
