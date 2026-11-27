package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTLSKeylogRealHandshakes(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		k := newTLSKeylog(t.TempDir())
		k.setEnabled(true)
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
		srv.TLS = &tls.Config{MinVersion: version, MaxVersion: version, KeyLogWriter: k}
		srv.StartTLS()
		client := srv.Client()
		res, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		client.CloseIdleConnections()
		srv.Close()
		k.setEnabled(false)
		files, _ := filepath.Glob(filepath.Join(k.dir, "*.keys"))
		if len(files) != 1 {
			t.Fatalf("files: %v", files)
		}
		data, _ := os.ReadFile(files[0])
		label := "CLIENT_RANDOM"
		if version == tls.VersionTLS13 {
			label = "CLIENT_TRAFFIC_SECRET_0"
		}
		if !strings.Contains(string(data), label+" ") {
			t.Fatalf("missing version %x secrets", version)
		}
		stat, _ := os.Stat(files[0])
		if stat.Mode().Perm() != 0600 {
			t.Fatal("unsafe permissions")
		}
	}
}
func TestTLSKeylogLimitsAndRecovery(t *testing.T) {
	dir := t.TempDir()
	k := newTLSKeylog(dir)
	k.Write([]byte("disabled\n"))
	if k.used != 0 {
		t.Fatal("disabled wrote data")
	}
	k.setEnabled(true)
	for i := 0; i < 8; i++ {
		if _, err := k.Write([]byte(strings.Repeat("x", int(keylogSegment)))); err != nil {
			t.Fatal(err)
		}
	}
	k.Write([]byte("overflow\n"))
	if k.used != keylogLimit || k.dropped != 1 {
		t.Fatalf("cap %d dropped %d", k.used, k.dropped)
	}
	recovered := newTLSKeylog(dir)
	if recovered.used != keylogLimit {
		t.Fatal("lost pending files")
	}
}
func TestTLSKeylogIOFailureDoesNotBreakTLS(t *testing.T) {
	k := newTLSKeylog(filepath.Join(t.TempDir(), "keys"))
	os.Remove(k.dir)
	k.setEnabled(true)
	p := []byte("CLIENT_RANDOM secret\n")
	n, err := k.Write(p)
	if n != len(p) || err != nil || k.dropped != 1 {
		t.Fatal("failure leaked to TLS")
	}
}
func TestTLSKeylogRestartSealsOpenFile(t *testing.T) {
	dir := t.TempDir()
	k := newTLSKeylog(dir)
	k.setEnabled(true)
	k.Write([]byte("test\n"))
	recovered := newTLSKeylog(dir)
	files, _ := filepath.Glob(filepath.Join(dir, "*.keys"))
	if recovered.used != 5 || len(files) != 1 {
		t.Fatal("restart recovery failed")
	}
}

func TestTLSKeylogUploadRetention(t *testing.T) {
	k := newTLSKeylog(t.TempDir())
	k.setEnabled(true)
	k.Write([]byte("secret\n"))
	k.setEnabled(false)
	k.uploadPending(context.Background(), func(context.Context, string, int64) error { return errors.New("offline") })
	if k.used != 7 || k.uploaded != 0 || k.retries != 1 {
		t.Fatal("failure discarded pending data")
	}
	k.uploadPending(context.Background(), func(_ context.Context, p string, size int64) error {
		data, err := os.ReadFile(p)
		if err != nil || int64(len(data)) != size {
			t.Fatal("wrong upload")
		}
		return nil
	})
	files, _ := filepath.Glob(filepath.Join(k.dir, "*.keys"))
	if k.used != 0 || k.uploaded != 1 || len(files) != 0 {
		t.Fatal("confirmed upload not removed")
	}
}
func TestTLSKeylogScope(t *testing.T) {
	cfg := &runtimeConfig{Scope: deviceScope{Mode: "selected", Addresses: []string{"192.0.2.100"}}}
	if !tlsKeylogAllowed(cfg, "192.0.2.100:1234") {
		t.Fatal("target excluded")
	}
	for _, remote := range []string{"192.0.2.101:1234", "127.0.0.1:1234", "malformed"} {
		if tlsKeylogAllowed(cfg, remote) {
			t.Fatal("non-target recorded")
		}
	}
	cfg.MonitorOnly = true
	if tlsKeylogAllowed(cfg, "192.0.2.100:1234") {
		t.Fatal("monitor mode recorded")
	}
}

func TestTLSKeylogTimeRotation(t *testing.T) {
	k := newTLSKeylog(t.TempDir())
	k.setEnabled(true)
	k.Write([]byte("secret\n"))
	k.started = time.Now().Add(-time.Minute)
	k.rotateDue()
	files, _ := filepath.Glob(filepath.Join(k.dir, "*.keys"))
	if k.active != "" || len(files) != 1 || k.used != 7 {
		t.Fatal("minute rotation failed")
	}
}

func TestTLSKeylogConfigPreservesHTTP2(t *testing.T) {
	keys := newTLSKeylog(t.TempDir())
	cfg := &runtimeConfig{Scope: deviceScope{Mode: "all"}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	base := &tls.Config{MinVersion: tls.VersionTLS12}
	base.GetConfigForClient = func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		return keylogTLSConfig(base, cfg, h.Conn.RemoteAddr().String(), keys), nil
	}
	srv.TLS = base
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	// Copy the generated certificate to the original config, as FSD does via GetCertificate.
	base.Certificates = srv.TLS.Certificates
	for _, proto := range []string{"h2", "http/1.1"} {
		conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{proto}})
		if err != nil {
			t.Fatal(err)
		}
		if conn.ConnectionState().NegotiatedProtocol != proto {
			t.Fatalf("ALPN lost: %s", proto)
		}
		conn.Close()
	}
}
