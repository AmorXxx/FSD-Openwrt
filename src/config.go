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
	Routes      []customRoute `yaml:"h2_routes" json:"h2_routes"`
	Domain      string        `yaml:"domain" json:"domain"`
	Certificate string        `yaml:"cert,omitempty" json:"cert,omitempty"`
	Key         string        `yaml:"key,omitempty" json:"key,omitempty"`
}

type dnsBlacklist struct {
	Block []string `yaml:"block" json:"block"`
	Allow []string `yaml:"allow" json:"allow"`
}

type customRoute struct {
	ResponseMode string `yaml:"response_mode,omitempty" json:"response_mode,omitempty"`
	Path         string `yaml:"path" json:"path"`
	JSON         string `yaml:"json" json:"json"`
}

type config struct {
	DNSRules       []dnsRule    `yaml:"dns_rules,omitempty" json:"dns_rules"`
	DNSPassthrough []string     `yaml:"dns_passthrough,omitempty" json:"-"`
	MonitorOnly    bool         `yaml:"monitor_only,omitempty" json:"monitor_only"`
	Scope          deviceScope  `yaml:"device_scope" json:"device_scope"`
	Version        int          `yaml:"version" json:"version"`
	DNS            []dnsRecord  `yaml:"dns,omitempty" json:"-"`
	Sites          []siteConfig `yaml:"h2_sites,omitempty" json:"h2_sites,omitempty"`
	Blacklist      dnsBlacklist `yaml:"dns_blacklist,omitempty" json:"-"`
}

func decodeConfig(data []byte) (config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return config{}, err
	}
	if len(root.Content) > 0 && root.Content[0].Kind == yaml.MappingNode {
		for i := 0; i < len(root.Content[0].Content); i += 2 {
			key := root.Content[0].Content[i].Value
			if key == "h2_routes" {
				return config{}, errors.New("顶层 h2_routes 已移除，请将接口配置放入各 h2_sites 站点的 h2_routes")
			}
			if key == "h2_site" {
				return config{}, errors.New("h2_site 已移除，请使用 h2_sites 列表")
			}
			if key == "dns" || key == "dns_blacklist" || key == "dns_passthrough" {
				return config{}, fmt.Errorf("旧字段 %s 已移除，请使用 dns_rules", key)
			}
		}
	}

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
	if input.Config.DNS != nil || input.Config.DNSPassthrough != nil || input.Config.Blacklist.Block != nil || input.Config.Blacklist.Allow != nil {
		return config{}, errors.New("旧 DNS 字段已移除，请使用 dns_rules")
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
	if cfg.MonitorOnly {
		if cfg.Scope.Mode != "all" || len(cfg.Scope.Addresses) > 0 {
			return errors.New("仅监控模式要求 device_scope.mode 为 all，addresses 为空")
		}
		if len(cfg.DNSRules)+len(cfg.DNSPassthrough)+len(cfg.DNS)+len(cfg.Blacklist.Block)+len(cfg.Blacklist.Allow)+len(cfg.Sites) > 0 {
			return errors.New("仅监控模式不能配置 DNS 改写、黑白名单、HTTPS 域名或接口")
		}
		return nil
	}
	if len(cfg.Sites) == 0 || len(cfg.Sites) > 16 {
		return errors.New("h2_sites 必须包含 1 至 16 个 HTTPS 站点")
	}
	cfg.Sites = append([]siteConfig(nil), cfg.Sites...)
	for i := range cfg.Sites {
		cfg.Sites[i].Domain = domainName(cfg.Sites[i].Domain)
	}
	seenSites := make(map[string]bool)
	for _, site := range cfg.sites() {
		if !validSiteDomain(site.Domain) || seenSites[site.Domain] {
			return errors.New("invalid or duplicate HTTPS domain")
		}
		seenSites[site.Domain] = true
		for _, path := range []string{site.Certificate, site.Key} {
			if path != "" && (!filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || strings.Contains(path, "-----BEGIN")) {
				return fmt.Errorf("TLS cert/key for %s must be absolute file paths; inline PEM is no longer supported", site.Domain)
			}
		}
		if (site.Certificate == "") != (site.Key == "") {
			return errors.New("TLS certificate and key must be supplied together")
		}
	}
	if len(cfg.DNSPassthrough) > 128 || len(cfg.DNS) > 16 || len(cfg.Blacklist.Block) > 128 || len(cfg.Blacklist.Allow) > 128 {
		return errors.New("limit exceeded for DNS, routes or blacklist")
	}
	if err := cfg.validateDNSRules(); err != nil {
		return err
	}
	for _, rule := range cfg.DNSRules {
		if seenSites[rule.Name] {
			return fmt.Errorf("域名配置冲突：%s 同时出现在 h2_sites 和 dns_rules；请只保留一处同名配置", rule.Name)
		}
	}
	seenPassthrough := make(map[string]bool)
	for i, name := range cfg.DNSPassthrough {
		name = domainName(name)
		if !validSiteDomain(name) || seenPassthrough[name] {
			return fmt.Errorf("invalid or duplicate DNS passthrough entry at index %d", i+1)
		}
		cfg.DNSPassthrough[i] = name
		seenPassthrough[name] = true
	}
	seenDNS := make(map[string]bool)
	for i := range cfg.DNS {
		entry := &cfg.DNS[i]
		entry.Name = domainName(entry.Name)
		if entry.IP == "" {
			entry.IP = "ap"
		}
		if !validSiteDomain(entry.Name) || seenDNS[entry.Name] ||
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
	for i := range cfg.Sites {
		cfg.Sites[i].Routes = append([]customRoute{}, cfg.Sites[i].Routes...)
		if err := validateSiteRoutes(cfg.Sites[i].Routes); err != nil {
			return fmt.Errorf("HTTPS 站点 %s: %w", cfg.Sites[i].Domain, err)
		}
	}

	return nil
}

func validateSiteRoutes(routes []customRoute) error {
	if len(routes) > 8 {
		return errors.New("每个站点最多 8 个接口")
	}
	seenRoutes := make(map[string]bool)
	for i := range routes {
		route := &routes[i]
		parsed, err := url.ParseRequestURI(route.Path)
		path := route.Path
		if (path != "*" && (err != nil || !strings.HasPrefix(path, "/") || parsed.Path != route.Path || parsed.RawQuery != "" || strings.Contains(path, "//"))) || !validRoutePattern(path) || len(path) > 128 || seenRoutes[path] {
			return fmt.Errorf("invalid or duplicate custom route at index %d", i+1)
		}
		switch route.ResponseMode {
		case "", "json":
			route.ResponseMode = ""
		case "close":
			if route.JSON == "" {
				route.JSON = "{}"
			}
		default:
			return fmt.Errorf("invalid response_mode at route index %d", i+1)
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
	// Only file references are stored; certificate and private key bytes are never exported.
	cfg.Sites = append([]siteConfig(nil), cfg.Sites...)
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
		if err == nil && certificateCoversSite(leaf, site.Domain) == nil && leaf.VerifyHostname(lanIP.String()) == nil &&
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
	if cfg.MonitorOnly {
		return nil
	}
	return cfg.Sites
}

func (cfg config) primarySite() siteConfig {
	if cfg.MonitorOnly {
		return siteConfig{Domain: "fsd-monitor.home.arpa"}
	}
	return cfg.sites()[0]
}

func customCertificate(site siteConfig) (*tls.Certificate, error) {
	for _, path := range []string{site.Certificate, site.Key} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
			return nil, fmt.Errorf("TLS cert/key for %s must be absolute file paths", site.Domain)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("TLS file for %s: %w", site.Domain, err)
		}
		if !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
			return nil, fmt.Errorf("TLS file for %s must be a regular file no larger than 4 MiB", site.Domain)
		}
	}
	pair, err := tls.LoadX509KeyPair(site.Certificate, site.Key)
	if err != nil {
		return nil, fmt.Errorf("TLS certificate for %s: %w", site.Domain, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if err := certificateCoversSite(leaf, site.Domain); err != nil {
		return nil, fmt.Errorf("TLS certificate does not cover %s: %w", site.Domain, err)
	}
	return &pair, nil
}

// Preserve explicit empty dns_rules across save/reload without adding the field
// to untouched legacy YAML files.
func (cfg config) MarshalYAML() (any, error) {
	type plainConfig config
	var node yaml.Node
	if err := node.Encode(plainConfig(cfg)); err != nil {
		return nil, err
	}
	if cfg.DNSRules != nil && len(cfg.DNSRules) == 0 {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "dns_rules"}, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
	}
	return &node, nil
}
