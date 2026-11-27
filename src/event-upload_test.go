package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testUploader() *eventUploader {
	return &eventUploader{limit: eventQueueLimit, wake: make(chan struct{}, 1), cfg: uploadSettings{DeviceID: "mu300"}}
}
func TestEventRequestStatesAndRawBody(t *testing.T) {
	u := testUploader()
	full := "binary\x00\xff\n\"content"
	r := httptest.NewRequest("POST", "https://example.com/a?x=1", strings.NewReader(full))
	r.RemoteAddr = "192.0.2.10:5030"
	u.record(r)
	r = httptest.NewRequest("POST", "https://example.com/a", nil)
	r.Body = io.NopCloser(io.MultiReader(strings.NewReader("partial"), brokenBody{}))
	r.RemoteAddr = "192.0.2.10:5031"
	u.record(r)
	u.handshakeFailure("192.0.2.10:5032", "example.com", "remote error: tls: bad certificate")
	if len(u.queue) != 3 || u.queue[0].Success != "REQUEST_RECEIVED" || u.queue[1].Success != "REQUEST_INCOMPLETE" || u.queue[2].Success != "TLS_HANDSHAKE_FAILED" {
		t.Fatal(u.queue)
	}
	if string(u.queue[0].chunks[0]) != full || string(u.queue[1].chunks[0]) != "partial" || u.queue[2].Path != "" || len(u.queue[2].chunks) != 0 {
		t.Fatal("raw body/state mismatch")
	}
	if u.queue[0].Path != "/a?x=1" {
		t.Fatal(u.queue[0])
	}
}
func TestEventQueueLimit(t *testing.T) {
	u := testUploader()
	u.limit = eventMetadataBudget + 8
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("POST", "https://example.com/a", strings.NewReader("data"))
		u.record(r)
	}
	if len(u.queue) != 1 || u.dropped != 2 || u.used > u.limit {
		t.Fatal(u.status())
	}
	u.record(httptest.NewRequest("POST", "https://example.com/a", strings.NewReader(strings.Repeat("x", 20))))
	if len(u.queue) != 0 || u.used != 0 || u.dropped != 4 {
		t.Fatal("oversized event was not dropped whole", u.status())
	}
}
func TestEventUploadAcknowledgement(t *testing.T) {
	u := testUploader()
	u.token = strings.Repeat("a", 64)
	u.record(httptest.NewRequest("POST", "https://example.com/a", strings.NewReader("\x00\xffbody")))
	event := u.queue[0]
	calls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+u.token {
			t.Error("missing authentication")
		}
		reader := bufio.NewReader(r.Body)
		line, _ := reader.ReadBytes('\n')
		var meta requestEvent
		if err := json.Unmarshal(line, &meta); err != nil || meta.ID != event.ID {
			t.Error("event ID changed")
		}
		body, _ := io.ReadAll(reader)
		if string(body) != "\x00\xffbody" {
			t.Error("body changed")
		}
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		if calls == 2 {
			io.WriteString(w, `{"event_id":"wrong","stored":true}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"event_id": meta.ID, "stored": true})
	}))
	defer srv.Close()
	u.cfg.Endpoint = srv.URL + "/v1/events"
	u.client = srv.Client()
	for i := 1; i <= 3; i++ {
		done, err := u.upload(context.Background(), event)
		if i < 3 && (done || err == nil) {
			t.Fatal("accepted failed/invalid acknowledgement")
		}
		if i == 3 && (!done || err != nil) {
			t.Fatal(done, err)
		}
	}
}
func TestTLSFailureHasSNI(t *testing.T) {
	app := scopeTestApp(t)
	app.eventUpload = testUploader()
	tracker := &tlsEventLog{names: make(map[string]string), app: app, output: io.Discard}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(app.publicSite))
	srv.Config.ConnContext = tracker.connection
	srv.Config.ConnState = tracker.state
	srv.Config.ErrorLog = log.New(tracker, "", 0)
	srv.StartTLS()
	defer srv.Close()
	// Force GetCertificate to run even though the test server has a default certificate.
	pair := srv.TLS.Certificates[0]
	srv.TLS.GetCertificate = func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		tracker.hello(h.Conn, h.ServerName)
		return &pair, nil
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", srv.Listener.Addr().String(), &tls.Config{ServerName: "failed.example", MinVersion: tls.VersionTLS12})
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		app.eventUpload.mu.Lock()
		n := len(app.eventUpload.queue)
		app.eventUpload.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	app.eventUpload.mu.Lock()
	defer app.eventUpload.mu.Unlock()
	if len(app.eventUpload.queue) != 1 {
		t.Fatal("missing TLS failure")
	}
	e := app.eventUpload.queue[0]
	if e.Success != "TLS_HANDSHAKE_FAILED" || e.SNI != "failed.example" || e.Domain != "failed.example" || e.Path != "" {
		t.Fatal(e)
	}
}

func TestEventWorkerReleasesMemoryOnAck(t *testing.T) {
	u := testUploader()
	u.token = strings.Repeat("x", 64)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader := bufio.NewReader(r.Body)
		line, _ := reader.ReadBytes('\n')
		var meta requestEvent
		json.Unmarshal(line, &meta)
		n, err := io.Copy(io.Discard, reader)
		if err != nil || n != 2*1024*1024 {
			t.Error("large request was not complete", n, err)
		}
		json.NewEncoder(w).Encode(map[string]any{"event_id": meta.ID, "stored": true})
	}))
	defer srv.Close()
	u.client = srv.Client()
	u.cfg.Endpoint = srv.URL + "/v1/events"
	u.record(httptest.NewRequest("POST", "https://example.com/a", strings.NewReader(strings.Repeat("x", 2*1024*1024))))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		saved := u.saved
		u.mu.Unlock()
		if saved == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.saved != 1 || u.used != 0 || u.inflight || len(u.queue) != 0 {
		t.Fatal("ack did not release queue")
	}
}
