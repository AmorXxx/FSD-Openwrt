package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const fsdSocket = "/var/run/fsd-service/fsd.sock"

func (app *application) startLocalAdministration() error {
	directory := filepath.Dir(fsdSocket)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(fsdSocket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("FSD socket path already exists")
		}
		socket, dialErr := net.DialTimeout("unix", fsdSocket, 300*time.Millisecond)
		if dialErr == nil {
			socket.Close()
			return errors.New("FSD socket is already active")
		}
		if err := os.Remove(fsdSocket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", fsdSocket)
	if err != nil {
		return err
	}
	if err := os.Chmod(fsdSocket, 0600); err != nil {
		listener.Close()
		return err
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			app.adminRoutes(w, r, true)
		}),
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 12 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 10 * time.Second,
	}
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("FSD local management: %v", err)
		}
	}()
	return nil
}

func runFSDRPC(args []string) {
	reply := func(value any) {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(value)
	}
	if len(args) == 1 && args[0] == "list" {
		reply(map[string]any{
			"installation": struct{}{}, "status": struct{}{}, "config": struct{}{}, "monitor": struct{}{},
			"probe": struct{}{}, "bench": struct{}{}, "editor": struct{}{},
			"checks":           map[string]string{"group": ""},
			"validate":         map[string]string{"yaml": ""},
			"apply":            map[string]string{"yaml": ""},
			"profiles":         struct{}{},
			"profile_document": map[string]string{"name": ""},
			"profile_action":   map[string]string{"draft": ""},
			"save_editor":      map[string]string{"draft": ""},
			"block":            map[string]string{"action": "", "domain": ""},
			"monitor_control":  map[string]string{"action": ""},
		})
		return
	}
	if len(args) != 2 || args[0] != "call" {
		reply(map[string]string{"error": "Unknown FSD RPC operation"})
		return
	}
	input := struct{ YAML, Action, Domain, Group, Draft, Name string }{}
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 2*1024*1024)).Decode(&input); err != nil && err != io.EOF {
		reply(map[string]string{"error": "Invalid FSD RPC arguments"})
		return
	}
	if args[1] == "installation" {
		reply(installationState("/etc/fsd-package/active", fsdSocket))
		return
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: 2 * time.Second}
			return dialer.DialContext(ctx, "unix", fsdSocket)
		},
	}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 15 * time.Second}
	request := func(method, endpoint, contentType, body string) (json.RawMessage, error) {
		req, err := http.NewRequest(method, "http://fsd"+endpoint, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, errors.New("FSD 后端暂不可用")
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 524288))
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s", strings.TrimSpace(string(data)))
		}
		if !json.Valid(data) {
			return json.Marshal(string(data))
		}
		return data, nil
	}
	var data json.RawMessage
	var err error
	switch args[1] {
	case "status":
		data, err = request("GET", "/api/status", "", "")
	case "config":
		data, err = request("GET", "/api/config", "", "")
	case "profiles":
		data, err = request("GET", "/api/fsd/profiles", "", "")
	case "profile_document":
		data, err = request("GET", "/api/fsd/profiles?"+url.Values{"name": {input.Name}}.Encode(), "", "")
	case "profile_action":
		data, err = request("POST", "/api/fsd/profiles", "application/json", input.Draft)
	case "editor":
		data, err = request("GET", "/api/fsd/config-editor", "", "")
	case "save_editor":
		data, err = request("PUT", "/api/fsd/config-editor", "application/json", input.Draft)
	case "probe":
		data, err = request("GET", "/api/fsd/probe", "", "")
	case "checks":
		data, err = request("GET", "/api/fsd/checks?"+url.Values{"group": {input.Group}}.Encode(), "", "")
	case "validate":
		data, err = request("POST", "/api/fsd/validate", "application/yaml", input.YAML)
	case "bench":
		data, err = request("GET", "/api/mon/bench", "", "")
	case "monitor":
		snapshot := make(map[string]json.RawMessage)
		for _, section := range []struct{ name, path string }{
			{"live", "/api/mon/live"}, {"dns", "/api/mon/dns"}, {"logs", "/api/log"},
		} {
			part, partErr := request("GET", section.path, "", "")
			if partErr != nil {
				reply(map[string]string{"error": partErr.Error()})
				return
			}
			snapshot[section.name] = part
		}
		reply(snapshot)
		return
	case "apply":
		data, err = request("PUT", "/api/config", "application/yaml", input.YAML)
	case "block":
		form := url.Values{"action": {input.Action}, "domain": {input.Domain}}
		data, err = request("POST", "/api/mon/block", "application/x-www-form-urlencoded", form.Encode())
	case "monitor_control":
		form := url.Values{"action": {input.Action}}
		data, err = request("POST", "/api/mon/ctl", "application/x-www-form-urlencoded", form.Encode())
	default:
		err = errors.New("Unknown FSD RPC method")
	}
	if err != nil {
		reply(map[string]string{"error": err.Error()})
		return
	}
	reply(data)
}

// Available before the daemon is enabled so LuCI can render setup instructions.
func installationState(activePath, socketPath string) map[string]bool {
	_, err := os.Stat(activePath)
	enabled := err == nil
	socket, err := net.DialTimeout("unix", socketPath, 300*time.Millisecond)
	running := err == nil
	if socket != nil {
		socket.Close()
	}
	return map[string]bool{"enabled": enabled, "running": running}
}
