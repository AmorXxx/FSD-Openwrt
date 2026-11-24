package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallationState(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active")
	socket := filepath.Join(dir, "sock")
	if s := installationState(active, socket); s["enabled"] || s["running"] {
		t.Fatal(s)
	}
	if err := os.WriteFile(active, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if s := installationState(active, socket); !s["enabled"] || s["running"] {
		t.Fatal(s)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if s := installationState(active, socket); !s["enabled"] || !s["running"] {
		t.Fatal(s)
	}
}
