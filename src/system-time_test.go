package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestPOSIXSystemTimezone(t *testing.T) {
	for _, tc := range []struct {
		rule, date string
		offset     int
	}{
		{"CST-8", "2026-11-24T06:30:00Z", 8 * 3600},
		{"UTC0", "2026-11-24T06:30:00Z", 0},
		{"<+0545>-5:45", "2026-11-24T06:30:00Z", 5*3600 + 45*60},
		{"EST5EDT,M3.2.0,M11.1.0", "2026-01-15T12:00:00Z", -5 * 3600},
		{"EST5EDT,M3.2.0,M11.1.0", "2026-07-15T12:00:00Z", -4 * 3600},
	} {
		t.Run(tc.rule+tc.date, func(t *testing.T) {
			loc, err := posixLocation(tc.rule)
			if err != nil {
				t.Fatal(err)
			}
			at, _ := time.Parse(time.RFC3339, tc.date)
			_, offset := at.In(loc).Zone()
			if offset != tc.offset {
				t.Fatalf("offset=%d want %d", offset, tc.offset)
			}
		})
	}
}

func TestSystemClockReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TZ")
	c := systemClock{tzPath: path, localtimePath: filepath.Join(dir, "localtime")}
	for _, tc := range []struct {
		rule   string
		offset int
	}{{"CST-8", 28800}, {"UTC0", 0}, {"EST5EDT,M3.2.0,M11.1.0", -18000}} {
		if err := os.WriteFile(path, []byte(tc.rule+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		c.checked = time.Time{}
		now := c.now()
		loc, _ := posixLocation(tc.rule)
		_, want := time.Now().In(loc).Zone()
		_, got := now.Zone()
		if got != want {
			t.Fatalf("%s offset=%d want %d", tc.rule, got, want)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	c.checked = time.Time{}
	if _, offset := c.now().Zone(); offset != 0 {
		t.Fatal("missing system timezone should default to UTC")
	}
}

func TestSystemLogTimestamp(t *testing.T) {
	var out bytes.Buffer
	message := []byte("example event\n")
	n, err := (systemLogWriter{output: &out}).Write(message)
	if err != nil || n != len(message) {
		t.Fatalf("write=%d %v", n, err)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} example event\n$`).MatchString(out.String()) {
		t.Fatal(out.String())
	}
}
