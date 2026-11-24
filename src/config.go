package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type dnsRecord struct {
	Name string `yaml:"name" json:"name"`
	IP   string `yaml:"ip" json:"ip"`
}

type siteConfig struct {
	Domain      string `yaml:"domain" json:"domain"`
	Certificate string `yaml:"cert,omitempty" json:"-"`
	Key         string `yaml:"key,omitempty" json:"-"`
}

type dnsBlacklist struct {
	Block []string `yaml:"block" json:"block"`
	Allow []string `yaml:"allow" json:"allow"`
}

type customRoute struct {
	Path string `yaml:"path" json:"path"`
	JSON string `yaml:"json" json:"json"`
}

type config struct {
	Scope     deviceScope   `yaml:"device_scope" json:"device_scope"`
	Version   int           `yaml:"version" json:"version"`
	DNS       []dnsRecord   `yaml:"dns" json:"dns"`
	Site      siteConfig    `yaml:"h2_site,omitempty" json:"h2_site,omitempty"`
	Sites     []siteConfig  `yaml:"h2_sites,omitempty" json:"h2_sites,omitempty"`
	Blacklist dnsBlacklist  `yaml:"dns_blacklist" json:"dns_blacklist"`
	Routes    []customRoute `yaml:"h2_routes" json:"h2_routes"`
}

func decodeConfig(data []byte) (config, error) {
	var input struct {
		Config     config    `yaml:",inline"`
		LegacyWiFi yaml.Node `yaml:"wifi"` // Accepted for migration only; never applied or exported.
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&input); err != nil {
		return input.Config, err
	}
	if err := decoder.Decode(new(yaml.Node)); err != io.EOF {
		return input.Config, errors.New("YAML must contain one document")
	}
	return input.Config, input.Config.validate()
}

func (app *application) validateImport(data []byte) (config, error) {
	cfg, err := decodeConfig(data)
	if err != nil {
		return cfg, err
	}
	for _, site := range cfg.sites() {
		if site.Certificate != "" {
			if _, err := customCertificate(site); err != nil {
				return cfg, err
			}
		}
	}
	return cfg, nil
}

var domainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var wildcardPattern = regexp.MustCompile(`^[a-z0-9*?](?:[a-z0-9*?-]{0,61}[a-z0-9*?])?$`)

func domainName(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}

func validDomain(name string, wildcard bool) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	pattern := domainPattern
	if wildcard {
		pattern = wildcardPattern
	}
	for _, label := range strings.Split(name, ".") {
		if !pattern.MatchString(label) {
			return false
		}
	}
	return true
}

func (cfg *config) validate() error {
	if err := cfg.Scope.validate(); err != nil {
		return err
	}
	if cfg.Version != 1 {
		return errors.New("version must be 1")
	}
	if len(cfg.Sites) > 0 && cfg.Site != (siteConfig{}) {
		return errors.New("use either h2_site or h2_sites, not both")
	}
	if len(cfg.Sites) > 16 {
		return errors.New("at most 16 HTTPS domains are supported")
	}
	if len(cfg.Sites) > 0 {
		cfg.Sites = append([]siteConfig(nil), cfg.Sites...)
		for i := range cfg.Sites {
			cfg.Sites[i].Domain = domainName(cfg.Sites[i].Domain)
		}
	} else {
		cfg.Site.Domain = domainName(cfg.Site.Domain)
	}
	seenSites := make(map[string]bool)
	for _, site := range cfg.sites() {
		if !validDomain(site.Domain, false) || seenSites[site.Domain] {
			return errors.New("invalid or duplicate HTTPS domain")
		}
		seenSites[site.Domain] = true
		if (site.Certificate == "") != (site.Key == "") {
			return errors.New("TLS certificate and key must be supplied together")
		}
	}
	if len(cfg.DNS) > 16 || len(cfg.Routes) > 8 || len(cfg.Blacklist.Block) > 128 || len(cfg.Blacklist.Allow) > 128 {
		return errors.New("limit exceeded for DNS, routes or blacklist")
	}
	seenDNS := make(map[string]bool)
	for i := range cfg.DNS {
		entry := &cfg.DNS[i]
		entry.Name = domainName(entry.Name)
		if entry.IP == "" {
			entry.IP = "ap"
		}
		if !validDomain(entry.Name, false) || seenDNS[entry.Name] ||
			(entry.IP != "ap" && (net.ParseIP(entry.IP) == nil || net.ParseIP(entry.IP).To4() == nil)) {
			return fmt.Errorf("invalid or duplicate DNS entry at index %d", i+1)
		}
		seenDNS[entry.Name] = true
	}
	for i, rules := range [][]string{cfg.Blacklist.Block, cfg.Blacklist.Allow} {
		for index, rule := range rules {
			rules[index] = domainName(rule)
			if !validDomain(rules[index], i == 0) {
				return fmt.Errorf("invalid DNS blacklist rule at index %d", index+1)
			}
		}
	}
	seenRoutes := make(map[string]bool)
	for i := range cfg.Routes {
		route := &cfg.Routes[i]
		parsed, err := url.ParseRequestURI(route.Path)
		path := route.Path
		if err != nil || !strings.HasPrefix(path, "/") || parsed.Path != route.Path || parsed.RawQuery != "" ||
			strings.Contains(path, "//") || !validRoutePattern(path) || len(path) > 128 || seenRoutes[path] {
			return fmt.Errorf("invalid or duplicate custom route at index %d", i+1)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(route.JSON)); err != nil || compact.Len() > 8192 {
			return fmt.Errorf("invalid JSON or response larger than 8 KiB at index %d", i+1)
		}
		route.Path, route.JSON = path, compact.String()
		seenRoutes[path] = true
	}
	return nil
}

func (cfg config) publicConfig() config {
	cfg.Site.Certificate, cfg.Site.Key = "", ""
	cfg.Sites = append([]siteConfig(nil), cfg.Sites...)
	for i := range cfg.Sites {
		cfg.Sites[i].Certificate, cfg.Sites[i].Key = "", ""
	}
	return cfg
}

func writePrivate(path string, content []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func certificate(dir string, site siteConfig, lanIP net.IP) (*tls.Certificate, error) {
	if site.Certificate != "" {
		return customCertificate(site)
	}

	certPath, keyPath := filepath.Join(dir, "local.crt"), filepath.Join(dir, "local.key")
	if pair, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err == nil && leaf.VerifyHostname(site.Domain) == nil && leaf.VerifyHostname(lanIP.String()) == nil &&
			time.Until(leaf.NotAfter) > 24*time.Hour {
			return &pair, nil
		}
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: site.Domain},
		NotBefore:    now.Add(-24 * time.Hour), NotAfter: now.AddDate(2, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{site.Domain}, IPAddresses: []net.IP{lanIP},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		return nil, err
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	if err := writePrivate(keyPath, keyPEM); err != nil {
		return nil, err
	}
	if err := writePrivate(certPath, certPEM); err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	return &pair, err
}

func (cfg config) sites() []siteConfig {
	if len(cfg.Sites) != 0 {
		return cfg.Sites
	}
	return []siteConfig{cfg.Site}
}

func (cfg config) primarySite() siteConfig { return cfg.sites()[0] }

func customCertificate(site siteConfig) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair([]byte(site.Certificate), []byte(site.Key))
	if err != nil {
		return nil, fmt.Errorf("TLS certificate for %s: %w", site.Domain, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if err := leaf.VerifyHostname(site.Domain); err != nil {
		return nil, fmt.Errorf("TLS certificate does not cover %s: %w", site.Domain, err)
	}
	return &pair, nil
}

func (cfg *config) preserveCertificates(previous config) {
	old := make(map[string]siteConfig)
	for _, site := range previous.sites() {
		old[site.Domain] = site
	}
	inherit := func(site *siteConfig) {
		if site.Certificate == "" {
			site.Certificate, site.Key = old[site.Domain].Certificate, old[site.Domain].Key
		}
	}
	if len(cfg.Sites) == 0 {
		inherit(&cfg.Site)
	} else {
		for i := range cfg.Sites {
			inherit(&cfg.Sites[i])
		}
	}
}
