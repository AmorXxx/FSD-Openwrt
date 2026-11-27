package main

import "crypto/tls"

// RequestClientCert asks for a certificate without requiring one or verifying
// its chain. TLS still verifies possession of the corresponding private key.
func siteTLSConfig(base *tls.Config, cfg *runtimeConfig, hello *tls.ClientHelloInfo, keys *tlsKeylog) *tls.Config {
	next := keylogTLSConfig(base, cfg, hello.Conn.RemoteAddr().String(), keys)
	next.ClientAuth = tls.NoClientCert
	if cfg == nil || cfg.MonitorOnly {
		return next
	}
	name := hello.ServerName
	if name == "" {
		name = cfg.primarySite().Domain
	}
	selected := cfg.siteKey(name)
	for _, site := range cfg.Sites {
		if site.Domain == selected && site.RequestClientCert {
			next.ClientAuth = tls.RequestClientCert
			break
		}
	}
	return next
}
