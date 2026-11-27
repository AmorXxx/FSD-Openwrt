package main

import (
	"crypto/tls"
	"crypto/x509"
	"gopkg.in/yaml.v3"
	"net"
	"testing"
)

func TestOptionalClientCertificates(t *testing.T) {
	wildcard := testSite(t, "*.example.test")
	wildcard.RequestClientCert = true
	exact := testSite(t, "off.example.test")
	app := testApp(t, config{Version: 1, Sites: []siteConfig{wildcard, exact}})
	clientSite := testSite(t, "untrusted-client.example")
	clientPair, err := tls.LoadX509KeyPair(clientSite.Certificate, clientSite.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, host := range []string{"on.example.test", "off.example.test"} {
			for _, supply := range []bool{false, true} {
				serverConn, clientConn := net.Pipe()
				base := &tls.Config{SessionTicketsDisabled: true, MinVersion: version, MaxVersion: version, GetCertificate: app.getCertificate}
				base.GetConfigForClient = func(h *tls.ClientHelloInfo) (*tls.Config, error) {
					return siteTLSConfig(base, app.current.Load(), h, nil), nil
				}
				server := tls.Server(serverConn, base)
				requested := false
				roots := x509.NewCertPool()
				// Trust only this test server's generated leaf, never the client leaf.
				cert, err := app.getCertificate(&tls.ClientHelloInfo{ServerName: host})
				if err != nil {
					t.Fatal(err)
				}
				leaf, err := x509.ParseCertificate(cert.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				roots.AddCert(leaf)
				client := tls.Client(clientConn, &tls.Config{MinVersion: version, MaxVersion: version, ServerName: host, RootCAs: roots, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					requested = true
					if supply {
						return &clientPair, nil
					}
					return &tls.Certificate{}, nil
				}})
				done := make(chan error, 1)
				go func() { done <- server.Handshake() }()
				clientErr := client.Handshake()
				serverErr := <-done
				serverConn.Close()
				clientConn.Close()
				if clientErr != nil || serverErr != nil {
					t.Fatalf("%x %s supply=%v: client=%v server=%v", version, host, supply, clientErr, serverErr)
				}
				want := host != "off.example.test"
				if requested != want {
					t.Fatalf("request=%v want %v", requested, want)
				}
				count := 0
				if want && supply {
					count = 1
				}
				if len(server.ConnectionState().PeerCertificates) != count || len(server.ConnectionState().VerifiedChains) != 0 {
					t.Fatal("unexpected client certificates or verification")
				}
			}
		}
	}
}

func TestClientCertificateEditorRoundTrip(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		site := siteConfig{Domain: "example.test", RequestClientCert: enabled, Routes: []customRoute{}}
		raw, err := yaml.Marshal(config{Version: 1, Sites: []siteConfig{site}})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := decodeConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := snapshotForEditor(cfg)
		if snapshot.Sites[0].RequestClientCert != enabled {
			t.Fatal("snapshot lost option")
		}
		items := []editorSiteUpdate{{Domain: site.Domain, RequestClientCert: enabled, Routes: []editorRouteUpdate{}}}
		sites, err := (editorUpdate{Sites: &items}).updatedSites(cfg.Sites)
		if err != nil || sites[0].RequestClientCert != enabled {
			t.Fatal("save lost option", err)
		}
	}
}
