package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// This harness is opt-in and only used in disposable Docker networks.
func TestScopeTrafficServer(t *testing.T) {
	if os.Getenv("FSD_SCOPE_TRAFFIC") != "server" {
		t.Skip("isolated traffic harness")
	}
	app := scopeTestApp(t)
	app.lanDevice = "eth0"
	cfg := app.current.Load().config
	cfg.Scope.Addresses = []string{"172.30.40.10", "fd73:30:40::10"}
	app.current.Store(app.runtimeSettings(cfg, app.current.Load().tls))
	app.dnsUpstream = "127.0.0.1:5355"
	if err := nft("table inet fw4 {\n" + string(app.guardFile(cfg.Scope)) + "}\n" + app.filterScript(cfg)); err != nil {
		t.Fatal(err)
	}
	var servers []*dns.Server
	for _, network := range []string{"udp", "tcp"} {
		for _, port := range []string{"53", "5355"} {
			handler := dns.Handler(dns.HandlerFunc(app.serveDNS))
			if port == "5355" {
				handler = dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
					r := new(dns.Msg)
					r.SetReply(q)
					r.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.ParseIP("203.0.113.9")}}
					_ = w.WriteMsg(r)
				})
			}
			server := &dns.Server{Addr: ":" + port, Net: network, Handler: handler}
			servers = append(servers, server)
			go server.ListenAndServe()
		}
	}
	defer func() {
		for _, s := range servers {
			_ = s.Shutdown()
		}
	}()
	site := &http.Server{Addr: ":443", Handler: http.HandlerFunc(app.publicSite)}
	go site.ListenAndServe()
	defer site.Close()
	leaseStop := make(chan struct{})
	leaseDone := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(leaseDone)
		for {
			_ = nft(clearForwardReady + fmt.Sprintf("add element inet fw4 fsd_forward_ready { %q timeout 5s }\n", app.lanDevice))
			select {
			case <-leaseStop:
				return
			case <-time.After(time.Second):
			}
		}
	}()
	stopLease := func() { once.Do(func() { close(leaseStop); <-leaseDone; _ = nft(clearForwardReady) }) }
	defer stopLease()
	finish := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ready") })
	mux.HandleFunc("/block", func(w http.ResponseWriter, r *http.Request) {
		if err := nft("add element inet fsd_service blocked_ipv4 { 172.30.41.10 }\nadd element inet fsd_service blocked_ipv6 { fd73:30:41::10 }"); err != nil {
			http.Error(w, err.Error(), 500)
		}
	})
	mux.HandleFunc("/unblock", func(w http.ResponseWriter, r *http.Request) {
		_ = nft("flush set inet fsd_service blocked_ipv4\nflush set inet fsd_service blocked_ipv6")
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		stopLease()
		for _, s := range servers {
			if s.Addr == ":53" {
				_ = s.Shutdown()
			}
		}
		_ = nft("delete table inet fsd_service")
		fmt.Fprint(w, "stopped")
	})
	mux.HandleFunc("/finish", func(w http.ResponseWriter, r *http.Request) { close(finish) })
	control := &http.Server{Addr: ":8080", Handler: mux}
	go control.ListenAndServe()
	defer control.Close()
	select {
	case <-finish:
	case <-time.After(3 * time.Minute):
		t.Fatal("traffic harness timed out")
	}
}
func TestScopeTrafficClient(t *testing.T) {
	if os.Getenv("FSD_SCOPE_TRAFFIC") != "client" {
		t.Skip("isolated traffic harness")
	}
	want := os.Getenv("FSD_EXPECT_DNS")
	for _, serverAddress := range []string{"172.30.40.2", "[fd73:30:40::2]"} {
		for _, network := range []string{"udp", "tcp"} {
			client := dns.Client{Net: network, Timeout: time.Second}
			q := new(dns.Msg)
			q.SetQuestion("site.home.arpa.", dns.TypeA)
			r, _, err := client.Exchange(q, serverAddress+":53")
			if want == "timeout" {
				if err == nil {
					t.Fatalf("%s unexpectedly reached FSD DNS", network)
				}
				continue
			}
			if err != nil || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != want {
				t.Fatalf("%s DNS got %v / %v, want %s", network, r, err, want)
			}
		}
		if want != "timeout" {
			req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://"+serverAddress+":443/a", nil)
			client := http.Client{Timeout: time.Second}
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			expected := http.StatusNotFound
			if want == "192.0.2.1" {
				expected = 200
			}
			if response.StatusCode != expected {
				t.Fatalf("HTTP got %d want %d", response.StatusCode, expected)
			}
		}
	}
	for _, wan := range []string{"172.30.41.10", "[fd73:30:41::10]"} {
		client := http.Client{Timeout: time.Second}
		response, err := client.Get("http://" + wan + ":8081/")
		if os.Getenv("FSD_EXPECT_WAN") == "blocked" {
			if err == nil {
				response.Body.Close()
				t.Fatal("target unexpectedly forwarded")
			}
			continue
		}
		if err != nil {
			t.Fatalf("nonblocked forwarding failed: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("WAN got %d", response.StatusCode)
		}
	}
}
