package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolRef(v bool) *bool { return &v }
func TestLoggingIndependentSwitchesAndMigration(t *testing.T) {
	app := &application{dir: t.TempDir()}
	os.WriteFile(filepath.Join(app.dir, "event-upload.json"), []byte(`{"enabled":true,"endpoint":"https://example.com/v1/events","device_id":"test","token_file":"/missing"}`), 0600)
	cfg, err := readLoggingSettings(app.dir)
	if err != nil || *cfg.LocalCSV {
		t.Fatal("legacy NAS must not enable CSV", err)
	}
	cfg.Enabled = false
	if err = app.saveLogging(loggingInput{uploadSettings: cfg}); err != nil {
		t.Fatal(err)
	}
	saved, _ := readLoggingSettings(app.dir)
	if *saved.LocalCSV || app.requestLogs != nil {
		t.Fatal("NAS disable enabled CSV")
	}
	cfg.LocalCSV = boolRef(true)
	if err = app.saveLogging(loggingInput{uploadSettings: cfg}); err != nil || app.requestLogs == nil {
		t.Fatal(err)
	}
	cfg.LocalCSV = boolRef(false)
	if err = app.saveLogging(loggingInput{uploadSettings: cfg}); err != nil || app.requestLogs != nil {
		t.Fatal(err)
	}
}
func TestLoggingDualDestination(t *testing.T) {
	for _, mode := range []string{"both", "csv-failure", "nas-full", "incomplete", "csv-only", "nas-only", "neither"} {
		t.Run(mode, func(t *testing.T) {
			app := &application{dir: t.TempDir(), eventUpload: testUploader(), requestLogs: &requestLog{dir: t.TempDir(), limit: 10 << 20}}
			body := strings.Repeat("完整,\"\n\x00\xff", 12000)
			if mode == "csv-failure" {
				app.requestLogs.limit = 1
			}
			if mode == "nas-full" {
				app.eventUpload.limit = 1
			}
			if mode == "csv-only" || mode == "neither" {
				app.eventUpload = nil
			}
			if mode == "nas-only" || mode == "neither" {
				app.requestLogs = nil
			}
			req := httptest.NewRequest("POST", "https://example.com/a?q=1", strings.NewReader(body))
			req.RemoteAddr = "192.0.2.1:5000"
			if mode == "incomplete" {
				req.Body = io.NopCloser(io.MultiReader(strings.NewReader(body), brokenBody{}))
			}
			app.recordRequest(req)
			if u := app.eventUpload; u != nil {
				if mode == "nas-full" {
					if u.dropped != 1 || len(u.queue) != 0 {
						t.Fatal("NAS overflow")
					}
				} else {
					if len(u.queue) != 1 || string(bytes.Join(u.queue[0].chunks, nil)) != body {
						t.Fatal("NAS body lost")
					}
					if mode == "incomplete" && u.queue[0].Success != "REQUEST_INCOMPLETE" {
						t.Fatal("wrong state")
					}
				}
			}
			if l := app.requestLogs; l != nil {
				if mode == "csv-failure" || mode == "incomplete" {
					if l.failed != 1 {
						t.Fatal("expected CSV failure")
					}
				} else {
					paths, _ := filepath.Glob(filepath.Join(l.dir, "*.csv"))
					if len(paths) != 1 {
						t.Fatal(paths)
					}
					data, _ := os.ReadFile(paths[0])
					rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
					if err != nil || len(rows) != 2 || rows[1][4] != body {
						t.Fatal("CSV body lost", err)
					}
				}
			}
		})
	}
}
func TestLoggingSaveSecretAndBusyQueue(t *testing.T) {
	app := &application{dir: t.TempDir()}
	secret := strings.Repeat("z", 48)
	cfg := uploadSettings{Enabled: true, LocalCSV: boolRef(false), Endpoint: "https://example.com/v1/events", DeviceID: "test"}
	if err := app.saveLogging(loggingInput{uploadSettings: cfg, Token: secret}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if app.uploadCancel != nil {
			app.uploadCancel()
		}
	}()
	state, err := app.loggingSnapshot()
	encoded, _ := json.Marshal(state)
	if err != nil || bytes.Contains(encoded, []byte(secret)) {
		t.Fatal("secret exposed", err)
	}
	saved, _ := readLoggingSettings(app.dir)
	info, _ := os.Stat(saved.TokenFile)
	if info.Mode().Perm() != 0600 {
		t.Fatal("secret permissions")
	}
	before, _ := os.ReadFile(filepath.Join(app.dir, "event-upload.json"))
	app.eventUpload.mu.Lock()
	app.eventUpload.used = 1
	app.eventUpload.mu.Unlock()
	cfg.Endpoint = "https://other.example/v1/events"
	if err := app.saveLogging(loggingInput{uploadSettings: cfg, Token: strings.Repeat("y", 48)}); err == nil {
		t.Fatal("changed busy uploader")
	}
	after, _ := os.ReadFile(filepath.Join(app.dir, "event-upload.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed save changed disk")
	}
	saved.LocalCSV = boolRef(true)
	if err := app.saveLogging(loggingInput{uploadSettings: saved}); err != nil || app.requestLogs == nil {
		t.Fatal("CSV toggle blocked", err)
	}
	app.eventUpload.mu.Lock()
	app.eventUpload.used = 0
	app.eventUpload.mu.Unlock()
	saved.Enabled = false
	saved.LocalCSV = boolRef(false)
	if err := app.saveLogging(loggingInput{uploadSettings: saved}); err != nil || app.eventUpload != nil || app.requestLogs != nil {
		t.Fatal("disable", err)
	}
}
func TestLoggingAPIRejectsNonLocalAndInvalidInput(t *testing.T) {
	app := &application{dir: t.TempDir()}
	for _, body := range []string{`{"local_csv":false,"enabled":false,"unknown":1}`, `{"local_csv":false} {}`, `{"enabled":false}`} {
		r := httptest.NewRequest("PUT", "/api/fsd/logging", strings.NewReader(body))
		w := httptest.NewRecorder()
		app.loggingAPI(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	w := httptest.NewRecorder()
	app.adminRoutes(w, httptest.NewRequest(http.MethodGet, "/api/fsd/logging", nil), false)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestLoggingConnectionProbe(t *testing.T) {
	secret := strings.Repeat("a", 48)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(401)
			return
		}
		data, _ := io.ReadAll(r.Body)
		if string(data) != "{}\n" {
			t.Error("probe attempted valid event")
		}
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"invalid_event"}`)
	}))
	defer server.Close()
	app := &application{dir: t.TempDir()}
	ca := filepath.Join(app.dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	cfg := uploadSettings{Endpoint: server.URL + "/v1/events", DeviceID: "test", CAFile: ca}
	if err := app.testLogging(loggingInput{uploadSettings: cfg, Token: secret}); err != nil {
		t.Fatal(err)
	}
	if err := app.testLogging(loggingInput{uploadSettings: cfg, Token: strings.Repeat("b", 48)}); err == nil {
		t.Fatal("accepted invalid token")
	}
	cfg.CAFile = ""
	if err := app.testLogging(loggingInput{uploadSettings: cfg, Token: secret}); err == nil {
		t.Fatal("accepted untrusted TLS")
	}
}
