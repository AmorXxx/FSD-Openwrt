package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const eventMetadataBudget int64 = 65536
const eventQueueLimit int64 = 64 * 1024 * 1024

type uploadSettings struct {
	TLSKeylog bool   `json:"tls_keylog"`
	Enabled   bool   `json:"enabled"`
	LocalCSV  *bool  `json:"local_csv,omitempty"`
	Endpoint  string `json:"endpoint"`
	DeviceID  string `json:"device_id"`
	TokenFile string `json:"token_file"`
	CAFile    string `json:"ca_file,omitempty"`
}
type requestEvent struct {
	ID         string `json:"event_id"`
	DeviceID   string `json:"device_id"`
	Time       string `json:"event_time"`
	Success    string `json:"success"`
	SourceIP   string `json:"source_ip"`
	SourcePort int    `json:"source_port,omitempty"`
	Domain     string `json:"domain,omitempty"`
	SNI        string `json:"sni,omitempty"`
	Method     string `json:"request_method,omitempty"`
	Path       string `json:"request_path,omitempty"`
	Error      string `json:"error,omitempty"`
	chunks     [][]byte
	cost       int64
}
type eventUploader struct {
	mu                      sync.Mutex
	capture                 sync.Mutex
	queue                   []*requestEvent
	used, limit             int64
	saved, dropped, retries uint64
	lastError, lastUpload   string
	cfg                     uploadSettings
	token                   string
	client                  *http.Client
	wake                    chan struct{}
	inflight                bool
}

func loadEventUploader(dir string) (*eventUploader, error) {
	data, err := os.ReadFile(filepath.Join(dir, "event-upload.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg uploadSettings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid event upload configuration")
	}
	if !cfg.Enabled {
		return nil, nil
	}
	return newEventUploader(cfg, "")
}

func newEventUploader(cfg uploadSettings, suppliedToken string) (*eventUploader, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || !strings.HasSuffix(endpoint.Path, "/v1/events") {
		return nil, errors.New("event endpoint must be an HTTPS /v1/events URL")
	}
	if cfg.DeviceID == "" || len(cfg.DeviceID) > 128 || !filepath.IsAbs(cfg.TokenFile) {
		return nil, errors.New("invalid event device ID or token path")
	}
	secret := strings.TrimSpace(suppliedToken)
	if secret == "" {
		token, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, err
		}
		secret = strings.TrimSpace(string(token))
	}
	if len(secret) < 32 || strings.ContainsAny(secret, "\r\n") {
		return nil, errors.New("invalid event upload token")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		if !filepath.IsAbs(cfg.CAFile) {
			return nil, errors.New("event CA path must be absolute")
		}
		ca, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("invalid event CA certificate")
		}
		tlsConfig.RootCAs = roots
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	transport.MaxConnsPerHost = 1
	return &eventUploader{cfg: cfg, token: secret, limit: eventQueueLimit, wake: make(chan struct{}, 1), client: &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func newRequestEvent(remote string) *requestEvent {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	host, port, _ := net.SplitHostPort(remote)
	n, _ := strconv.Atoi(port)
	return &requestEvent{ID: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), Time: systemNow().Format("2006-01-02 15:04:05.000000"), SourceIP: host, SourcePort: n}
}

// used includes queued, in-flight and currently collected payloads. Never evict
// the event being uploaded; only complete waiting events can be discarded.
func (u *eventUploader) reserve(n int64) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for u.used+n > u.limit && len(u.queue) > 0 {
		old := u.queue[0]
		u.queue[0] = nil
		u.queue = u.queue[1:]
		u.used -= old.cost
		u.dropped++
	}
	if u.used+n > u.limit {
		return false
	}
	u.used += n
	return true
}
func (u *eventUploader) drop(cost int64, reason string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.used -= cost
	u.dropped++
	u.lastError = reason
}
func (u *eventUploader) enqueue(e *requestEvent) {
	e.DeviceID = u.cfg.DeviceID
	meta, err := json.Marshal(e)
	if err != nil || len(meta)+1 > int(eventMetadataBudget) {
		u.drop(e.cost, "event metadata exceeds memory budget")
		return
	}
	u.mu.Lock()
	u.queue = append(u.queue, e)
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default:
	}
}
func (u *eventUploader) record(req *http.Request) {
	// Serializing collection bounds allocation overhead independently of HTTP/2 concurrency.
	u.capture.Lock()
	defer u.capture.Unlock()
	e := newRequestEvent(req.RemoteAddr)
	e.Domain = req.Host
	if host, _, err := net.SplitHostPort(e.Domain); err == nil {
		e.Domain = host
	}
	if req.TLS != nil {
		e.SNI = req.TLS.ServerName
	}
	e.Method = req.Method
	e.Path = req.URL.RequestURI()
	e.Success = "REQUEST_RECEIVED"
	if !u.reserve(eventMetadataBudget) {
		u.drop(0, "memory queue full")
		if req.Body != nil {
			io.Copy(io.Discard, req.Body)
		}
		return
	}
	e.cost = eventMetadataBudget
	if req.Body != nil {
		chunk := make([]byte, 32768)
		for {
			n, err := req.Body.Read(chunk)
			if n > 0 {
				if !u.reserve(int64(n)) {
					u.drop(e.cost, "request cannot fit memory queue; event discarded without truncation")
					e.chunks = nil
					io.Copy(io.Discard, req.Body)
					return
				}
				e.cost += int64(n)
				e.chunks = append(e.chunks, append([]byte(nil), chunk[:n]...))
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				e.Success = "REQUEST_INCOMPLETE"
				e.Error = err.Error()
				break
			}
		}
	}
	u.enqueue(e)
}
func (u *eventUploader) handshakeFailure(remote, sni, reason string) {
	e := newRequestEvent(remote)
	if net.ParseIP(e.SourceIP) == nil {
		return
	}
	e.Success = "TLS_HANDSHAKE_FAILED"
	e.SNI = sni
	e.Domain = sni
	e.Error = reason
	if !u.reserve(eventMetadataBudget) {
		u.drop(0, "memory queue full")
		return
	}
	e.cost = eventMetadataBudget
	u.enqueue(e)
}
func (u *eventUploader) upload(ctx context.Context, e *requestEvent) (bool, error) {
	meta, err := json.Marshal(e)
	if err != nil {
		return true, err
	}
	meta = append(meta, '\n')
	readers := []io.Reader{bytes.NewReader(meta)}
	length := int64(len(meta))
	for _, chunk := range e.chunks {
		readers = append(readers, bytes.NewReader(chunk))
		length += int64(len(chunk))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.cfg.Endpoint, io.MultiReader(readers...))
	if err != nil {
		return true, err
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+u.token)
	res, err := u.client.Do(req)
	if err != nil {
		return false, errors.New("HTTPS event upload failed (network/TLS/timeout)")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return res.StatusCode == 400 || res.StatusCode == 413 || res.StatusCode == 422, fmt.Errorf("event receiver HTTP %d", res.StatusCode)
	}
	var ack struct {
		ID     string `json:"event_id"`
		Stored bool   `json:"stored"`
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&ack); err != nil || !ack.Stored || ack.ID != e.ID {
		return false, errors.New("invalid event storage acknowledgement")
	}
	return true, nil
}
func (u *eventUploader) run(ctx context.Context) {
	for {
		u.mu.Lock()
		var e *requestEvent
		if len(u.queue) > 0 {
			e = u.queue[0]
			u.queue[0] = nil
			u.queue = u.queue[1:]
			u.inflight = true
		}
		u.mu.Unlock()
		if e == nil {
			select {
			case <-ctx.Done():
				return
			case <-u.wake:
				continue
			}
		}
		for {
			done, err := u.upload(ctx, e)
			u.mu.Lock()
			if done {
				u.used -= e.cost
				u.inflight = false
				if err == nil {
					u.saved++
					u.lastUpload = systemNow().Format(displayTimeLayout)
					u.lastError = ""
				} else {
					u.dropped++
					u.lastError = err.Error()
				}
			} else {
				u.retries++
				u.lastError = err.Error()
			}
			u.mu.Unlock()
			if done {
				break
			}
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
func (u *eventUploader) status() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return map[string]any{"enabled": true, "queued": len(u.queue), "inflight": u.inflight, "buffer_bytes": u.used, "buffer_limit": u.limit, "uploaded": u.saved, "dropped": u.dropped, "retries": u.retries, "last_error": u.lastError, "last_upload": u.lastUpload}
}
func (app *application) uploadStatus() map[string]any {
	app.loggingMu.RLock()
	defer app.loggingMu.RUnlock()
	if app.eventUpload == nil {
		return map[string]any{"enabled": false, "local_csv": app.requestLogs != nil}
	}
	state := app.eventUpload.status()
	state["local_csv"] = app.requestLogs != nil
	return state
}
