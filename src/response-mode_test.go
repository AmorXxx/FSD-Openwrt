package main

import (
	"context"
	"crypto/tls"
	"encoding/csv"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCloseModeTLSProtocols(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "http1"
		if h2 {
			name = "h2"
		}
		t.Run(name, func(t *testing.T) {
			app := scopeTestApp(t)
			current := app.current.Load()
			current.Sites[0].Routes[0].ResponseMode = "close"
			app.requestLogs = &requestLog{dir: t.TempDir(), limit: 1 << 20}
			protocol := make(chan int, 2)
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				protocol <- r.ProtoMajor
				r.Host = "site.home.arpa"
				app.publicSite(w, r)
			}))
			srv.EnableHTTP2 = h2
			srv.Config.ConnContext = requestConnectionContext
			srv.StartTLS()
			defer srv.Close()
			client := srv.Client()
			client.Timeout = 3 * time.Second
			response, err := client.Post(srv.URL+"/a?actual=1", "application/json", strings.NewReader(`{"complete":"request"}`))
			if response != nil {
				response.Body.Close()
				t.Fatalf("unexpected HTTP response: %d", response.StatusCode)
			}
			if err == nil {
				t.Fatal("expected connection termination")
			}
			expected := 1
			if h2 {
				expected = 2
			}
			if got := <-protocol; got != expected {
				t.Fatal("wrong negotiated protocol", got)
			}
			files, _ := filepath.Glob(filepath.Join(app.requestLogs.dir, "*.csv"))
			if len(files) != 1 {
				t.Fatal(files)
			}
			data, _ := os.ReadFile(files[0])
			rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
			if err != nil || len(rows) != 2 || rows[1][3] != "/a?actual=1" || rows[1][4] != `{"complete":"request"}` {
				t.Fatal(rows, err)
			}
		})
	}
}

func TestResponseModeConfigEditor(t *testing.T) {
	cfg := scopeTestApp(t).current.Load().config
	cfg.Sites[0].Routes = []customRoute{{Path: "/close", ResponseMode: "close"}}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotForEditor(cfg)
	if snapshot.Sites[0].Routes[0].ResponseMode != "close" {
		t.Fatal(snapshot)
	}
	// An older UI referencing unchanged JSON must preserve the existing mode.
	input := editorUpdate{Routes: []editorRouteUpdate{{Path: "/renamed", Source: "/close"}}}
	routes, err := input.updatedRoutes(cfg.Sites[0].Routes)
	if err != nil || routes[0].ResponseMode != "close" {
		t.Fatal(routes, err)
	}
	normal := ""
	input.Routes[0].ResponseMode = &normal
	routes, err = input.updatedRoutes(cfg.Sites[0].Routes)
	if err != nil || routes[0].ResponseMode != "" || routes[0].JSON != "{}" {
		t.Fatal(routes, err)
	}
	cfg.Sites[0].Routes[0].ResponseMode = "invalid"
	if cfg.validate() == nil {
		t.Fatal("accepted invalid mode")
	}
}

func TestCloseCatchAllDiagnosticHandshake(t *testing.T) {
	app := scopeTestApp(t)
	current := app.current.Load()
	current.Sites[0].Routes = []customRoute{{Path: "*", ResponseMode: "close", JSON: "{}"}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(app.publicSite))
	srv.EnableHTTP2 = true
	srv.Config.ConnContext = requestConnectionContext
	srv.StartTLS()
	defer srv.Close()
	app.lanAddr = netip.MustParseAddr("127.0.0.1")
	app.sitePort = srv.Listener.Addr().(*net.TCPAddr).Port
	pair := &tls.Certificate{Certificate: [][]byte{srv.Certificate().Raw}}
	current.tls = pair
	current.certificates = map[string]*tls.Certificate{current.primarySite().Domain: pair}
	probes := app.protocolProbe(context.Background(), current)
	if len(probes) != 2 {
		t.Fatal(probes)
	}
	for _, probe := range probes {
		if !probe.OK || !probe.TLSOnly || probe.HTTPStatus != 0 {
			t.Fatal(probe)
		}
	}
	checks := app.checkRoutes(context.Background(), current)
	if len(checks) != 1 || checks[0].Status != "skip" {
		t.Fatal(checks)
	}
}

func TestHTTP2ResetPreservesConcurrentStreamAndConnection(t *testing.T) {
	app := scopeTestApp(t)
	app.current.Load().Sites[0].Routes[0].ResponseMode = "close"
	app.requestLogs = &requestLog{dir: t.TempDir(), limit: 1 << 20}
	release := make(chan struct{})
	peers := make(chan string, 3)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("expected HTTP/2")
		}
		peers <- r.RemoteAddr
		switch r.URL.Path {
		case "/slow":
			w.Write([]byte("before-"))
			w.(http.Flusher).Flush()
			select {
			case <-release:
				w.Write([]byte("after"))
			case <-r.Context().Done():
				t.Error("unrelated stream cancelled")
			}
		case "/next":
			w.Write([]byte("reused"))
		default:
			r.Host = "site.home.arpa"
			app.publicSite(w, r)
		}
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnContext = requestConnectionContext
	srv.StartTLS()
	defer srv.Close()
	defer close(release)
	client := srv.Client()
	client.Timeout = 4 * time.Second
	slow, err := client.Get(srv.URL + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Body.Close()
	peer := <-peers
	response, err := client.Post(srv.URL+"/a", "text/plain", strings.NewReader("logged before reset"))
	if response != nil {
		response.Body.Close()
		t.Fatal("received HTTP response instead of reset")
	}
	if err == nil || !strings.Contains(err.Error(), "stream error") {
		t.Fatalf("expected stream reset: %v", err)
	}
	if got := <-peers; got != peer {
		t.Fatal("requests did not share a connection")
	}
	// Let the already active stream finish after the reset.
	release <- struct{}{}
	body, err := io.ReadAll(slow.Body)
	if err != nil || string(body) != "before-after" {
		t.Fatalf("other stream interrupted: %q %v", body, err)
	}
	next, err := client.Get(srv.URL + "/next")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Body.Close()
	body, err = io.ReadAll(next.Body)
	if err != nil || string(body) != "reused" {
		t.Fatal(string(body), err)
	}
	if got := <-peers; got != peer {
		t.Fatal("connection was replaced after reset")
	}
	files, _ := filepath.Glob(filepath.Join(app.requestLogs.dir, "*.csv"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	data, _ := os.ReadFile(files[0])
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil || len(rows) != 2 || rows[1][4] != "logged before reset" {
		t.Fatal(rows, err)
	}
}
