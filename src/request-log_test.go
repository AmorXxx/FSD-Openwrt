package main

import (
	"encoding/csv"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRequestLogFullBody(t *testing.T) {
	l := &requestLog{dir: t.TempDir(), limit: 10 << 20}
	body := strings.Repeat("完整,\"内容\"\n", 150000)
	r := httptest.NewRequest("POST", "https://demo.example/a/b?q=x", strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:5000"
	if err := l.record(r); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(l.dir, "*.csv"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	f, _ := os.Open(files[0])
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("CSV: %v", err)
	}
	if rows[1][1] != "192.0.2.10" || rows[1][2] != "demo.example" || rows[1][3] != "/a/b?q=x" || rows[1][4] != body {
		t.Fatal("request did not round trip")
	}
	info, _ := os.Stat(files[0])
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}
func TestRequestLogRotationAndFailure(t *testing.T) {
	l := &requestLog{dir: t.TempDir(), limit: 500}
	for i := 0; i < 8; i++ {
		if err := l.record(httptest.NewRequest("POST", "https://demo.example/", strings.NewReader(strings.Repeat("a", 100)))); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(l.dir, "*.csv"))
	var size int64
	for _, f := range files {
		s, _ := os.Stat(f)
		size += s.Size()
	}
	if size > l.limit || len(files) >= 8 || len(files) == 0 {
		t.Fatal(size, files)
	}
	if err := l.record(httptest.NewRequest("POST", "https://demo.example/", strings.NewReader(strings.Repeat("a", 1000)))); err == nil {
		t.Fatal("accepted oversized record")
	}
	parts, _ := filepath.Glob(filepath.Join(l.dir, "*.part"))
	if len(parts) != 0 {
		t.Fatal(parts)
	}
	r := httptest.NewRequest("POST", "https://demo.example/", nil)
	r.Body = io.NopCloser(brokenBody{})
	if err := l.record(r); err == nil {
		t.Fatal("accepted incomplete body")
	}
	if l.failed != 2 {
		t.Fatal(l.failed)
	}
}

type brokenBody struct{}

func (brokenBody) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRequestLogOnlyMatchedRequests(t *testing.T) {
	app := scopeTestApp(t)
	app.requestLogs = &requestLog{dir: t.TempDir(), limit: 1 << 20}
	for _, tc := range []struct {
		path, source string
		status       int
	}{
		{"/a", "192.0.2.10:4000", 200},
		{"/missing", "192.0.2.10:4000", 404},
		{"/a", "192.0.2.99:4000", 404},
	} {
		r := httptest.NewRequest("POST", "https://site.home.arpa"+tc.path, strings.NewReader("body"))
		r.RemoteAddr = tc.source
		w := httptest.NewRecorder()
		app.publicSite(w, r)
		if w.Code != tc.status {
			t.Fatal(w.Code)
		}
	}
	if app.requestLogs.saved != 1 {
		t.Fatal(app.requestLogs.saved)
	}
	// Disk/logging failures must not change the configured JSON response.
	app.requestLogs.limit = 1
	r := httptest.NewRequest("POST", "https://site.home.arpa/a", strings.NewReader("body"))
	r.RemoteAddr = "192.0.2.10:4000"
	w := httptest.NewRecorder()
	app.publicSite(w, r)
	if w.Code != 200 || w.Body.String() != `{"ok":true}` || app.requestLogs.failed != 1 {
		t.Fatal(w.Code, w.Body.String(), app.requestLogs.failed)
	}
}

func TestRequestLogAppendAndRestart(t *testing.T) {
	dir := t.TempDir()
	l := &requestLog{dir: dir, limit: 1 << 20}
	for _, body := range []string{"first", "second\n\"line\""} {
		if err := l.record(httptest.NewRequest("POST", "https://demo.example/a", strings.NewReader(body))); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.csv"))
	if len(files) != 1 {
		t.Fatal("expected one appended CSV", files)
	}
	// A new logger must continue the same file after restart.
	l = &requestLog{dir: dir, limit: 1 << 20}
	if err := l.record(httptest.NewRequest("POST", "https://demo.example/b", strings.NewReader("third"))); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(files[0])
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil || len(rows) != 4 || rows[1][4] != "first" || rows[2][4] != "second\n\"line\"" || rows[3][4] != "third" {
		t.Fatal(rows, err)
	}
	// Simulate a process crash after writing part of an uncommitted row.
	if err = l.saveCheckpoint(appendCheckpoint{File: filepath.Base(files[0]), Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("\"broken row")
	f.Close()
	l = &requestLog{dir: dir, limit: 1 << 20}
	if err = l.record(httptest.NewRequest("POST", "https://demo.example/c", strings.NewReader("fourth"))); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(files[0])
	rows, err = csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil || len(rows) != 5 || rows[4][4] != "fourth" {
		t.Fatal(rows, err)
	}
	// A partially received request must leave all prior rows byte-for-byte intact.
	r := httptest.NewRequest("POST", "https://demo.example/c", nil)
	r.Body = io.NopCloser(io.MultiReader(strings.NewReader(strings.Repeat("partial", 10000)), brokenBody{}))
	if err = l.record(r); err == nil {
		t.Fatal("accepted broken body")
	}
	after, _ := os.ReadFile(files[0])
	if string(after) != string(data) {
		t.Fatal("failed append corrupted existing rows")
	}
}

func TestRequestLogRotateAtRowBoundary(t *testing.T) {
	l := &requestLog{dir: t.TempDir(), limit: 10000, rotateBytes: 200}
	for i := 0; i < 5; i++ {
		if err := l.record(httptest.NewRequest("POST", "https://demo.example/a", strings.NewReader(strings.Repeat("x", 150)))); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(l.dir, "*.csv"))
	if len(files) < 2 {
		t.Fatal("did not rotate", files)
	}
	count := 0
	for _, name := range files {
		data, _ := os.ReadFile(name)
		rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
		if err != nil || len(rows) < 2 {
			t.Fatal(name, err)
		}
		for _, row := range rows[1:] {
			if row[4] != strings.Repeat("x", 150) {
				t.Fatal("split body")
			}
			count++
		}
	}
	if count != 5 {
		t.Fatal(count)
	}
}

func TestRequestLogConcurrentAppend(t *testing.T) {
	l := &requestLog{dir: t.TempDir(), limit: 1 << 20}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.record(httptest.NewRequest("POST", "https://demo.example/a", strings.NewReader("body,\"quoted\"\n"))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	files, _ := filepath.Glob(filepath.Join(l.dir, "*.csv"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	data, _ := os.ReadFile(files[0])
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil || len(rows) != 21 {
		t.Fatal(len(rows), err)
	}
	for _, row := range rows[1:] {
		if row[4] != "body,\"quoted\"\n" {
			t.Fatal(row)
		}
	}
}
