package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// Track SNI by connection address only until the handshake finishes. No TLS
// message parsing or private key access is needed; GetCertificate sees ClientHello.
type tlsEventLog struct {
	mu     sync.Mutex
	names  map[string]string
	app    *application
	output io.Writer
}

func (t *tlsEventLog) connection(ctx context.Context, c net.Conn) context.Context {
	t.mu.Lock()
	t.names[c.RemoteAddr().String()] = ""
	t.mu.Unlock()
	return requestConnectionContext(ctx, c)
}
func (t *tlsEventLog) hello(c net.Conn, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.names[c.RemoteAddr().String()]; ok {
		t.names[c.RemoteAddr().String()] = name
	}
}
func (t *tlsEventLog) state(c net.Conn, state http.ConnState) {
	if state == http.StateActive || state == http.StateClosed || state == http.StateHijacked {
		t.mu.Lock()
		delete(t.names, c.RemoteAddr().String())
		t.mu.Unlock()
	}
}

var tlsFailureLine = regexp.MustCompile(`^http: TLS handshake error from (\S+): (.*)$`)

func (t *tlsEventLog) Write(p []byte) (int, error) {
	match := tlsFailureLine.FindStringSubmatch(strings.TrimSpace(string(p)))
	if len(match) == 3 {
		t.mu.Lock()
		name, known := t.names[match[1]]
		delete(t.names, match[1])
		t.mu.Unlock()
		cfg := t.app.current.Load()
		t.app.loggingMu.RLock()
		if known && t.app.eventUpload != nil && cfg != nil && !cfg.MonitorOnly && t.app.scopedClient(cfg.Scope, match[1]) {
			t.app.eventUpload.handshakeFailure(match[1], name, match[2])
		}
		t.app.loggingMu.RUnlock()
	}
	return t.output.Write(p)
}
