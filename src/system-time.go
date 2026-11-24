package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const displayTimeLayout = "2006-01-02 15:04:05"

// OpenWrt writes its applied POSIX timezone to /etc/TZ. Go's time.Local
// does not read that file. A TZif v2 footer lets Go evaluate POSIX offsets
// and daylight-saving rules without requiring an installed zoneinfo package.
func posixLocation(rule string) (*time.Location, error) {
	var data bytes.Buffer
	for range 2 {
		data.WriteString("TZif2")
		data.Write(make([]byte, 15))
		for _, count := range []uint32{0, 0, 0, 0, 1, 4} {
			_ = binary.Write(&data, binary.BigEndian, count)
		}
		data.Write(make([]byte, 6)) // One UTC fallback type, no transitions.
		data.WriteString("UTC\x00")
	}
	data.WriteString("\n" + rule + "\n")
	return time.LoadLocationFromTZData("OpenWrt", data.Bytes())
}

type systemClock struct {
	sync.Mutex
	tzPath, localtimePath string
	checked               time.Time
	location              *time.Location
}

var deviceClock = systemClock{tzPath: "/etc/TZ", localtimePath: "/etc/localtime"}

func (c *systemClock) now() time.Time {
	c.Lock()
	defer c.Unlock()
	now := time.Now()
	// Re-read applied system configuration without restarting the service.
	if c.location == nil || now.Sub(c.checked) >= time.Second {
		c.checked = now
		c.location = time.UTC
		if data, err := os.ReadFile(c.tzPath); err == nil && strings.TrimSpace(string(data)) != "" {
			if location, err := posixLocation(strings.TrimSpace(string(data))); err == nil {
				c.location = location
			}
		} else if data, err := os.ReadFile(c.localtimePath); err == nil {
			if location, err := time.LoadLocationFromTZData("System", data); err == nil {
				c.location = location
			}
		}
	}
	return now.In(c.location)
}

func systemNow() time.Time { return deviceClock.now() }

// Standard Go logs and monitor logs share the same system-time prefix.
type systemLogWriter struct{ output io.Writer }

func (w systemLogWriter) Write(p []byte) (int, error) {
	_, err := io.WriteString(w.output, systemNow().Format(displayTimeLayout)+" "+string(p))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
