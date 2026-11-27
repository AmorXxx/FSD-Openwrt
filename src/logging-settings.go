package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Logging destinations are opt-in. Missing local_csv stays disabled; explicit
// saved switches are preserved independently.
func readLoggingSettings(dir string) (uploadSettings, error) {
	cfg := uploadSettings{TokenFile: filepath.Join(dir, "upload-token")}
	data, err := os.ReadFile(filepath.Join(dir, "event-upload.json"))
	if err != nil && !os.IsNotExist(err) {
		return cfg, err
	}
	if err == nil {
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err = d.Decode(&cfg); err != nil {
			return cfg, err
		}
		if d.Decode(new(any)) != io.EOF {
			return cfg, errors.New("invalid logging configuration")
		}
	}
	if cfg.LocalCSV == nil {
		enabled := false
		cfg.LocalCSV = &enabled
	}
	return cfg, nil
}

type loggingInput struct {
	uploadSettings
	Token string `json:"token,omitempty"`
}

func (app *application) loggingSnapshot() (map[string]any, error) {
	app.loggingMu.RLock()
	defer app.loggingMu.RUnlock()
	cfg, err := readLoggingSettings(app.dir)
	if err != nil {
		return nil, err
	}
	state := map[string]any{"enabled": false}
	if app.eventUpload != nil {
		state = app.eventUpload.status()
	}
	csv := map[string]any{"enabled": app.requestLogs != nil, "directory": filepath.Join(app.dir, "request-logs"), "limit": 100 * 1024 * 1024}
	if l := app.requestLogs; l != nil {
		l.mu.Lock()
		csv["saved"] = l.saved
		csv["failed"] = l.failed
		csv["last_error"] = l.lastError
		l.mu.Unlock()
	}
	keys := map[string]any{}
	if app.keylogs != nil {
		keys = app.keylogs.status()
	}
	token, err := os.ReadFile(cfg.TokenFile)
	return map[string]any{"tls_keylog": keys, "config": cfg, "token_present": err == nil && len(strings.TrimSpace(string(token))) >= 32, "upload": state, "csv": csv}, nil
}

func (app *application) saveLogging(input loggingInput) error {
	app.loggingMu.Lock()
	defer app.loggingMu.Unlock()
	old, err := readLoggingSettings(app.dir)
	if err != nil {
		return err
	}
	cfg := input.uploadSettings
	if cfg.LocalCSV == nil {
		return errors.New("必须明确设置本地 CSV 开关")
	}
	// Token paths are managed by the server, never supplied by the browser.
	cfg.TokenFile = old.TokenFile
	if cfg.TokenFile == "" {
		cfg.TokenFile = filepath.Join(app.dir, "upload-token")
	}
	token := strings.TrimSpace(input.Token)
	before, after := old, cfg
	before.TLSKeylog = false
	after.TLSKeylog = false
	before.LocalCSV = nil
	after.LocalCSV = nil
	changed := !reflect.DeepEqual(before, after) || token != ""
	var next *eventUploader
	if cfg.Enabled && changed {
		next, err = newEventUploader(cfg, token)
		if err != nil {
			return err
		}
		defer next.client.CloseIdleConnections()
	}
	if token != "" && (len(token) < 32 || strings.ContainsAny(token, "\r\n")) {
		return errors.New("上传令牌至少 32 个字符，不能包含换行")
	}
	if changed && app.eventUpload != nil {
		app.eventUpload.mu.Lock()
		busy := app.eventUpload.used != 0 || app.eventUpload.inflight
		app.eventUpload.mu.Unlock()
		if busy {
			return errors.New("仍有待上传日志，请等待缓存清空后修改 NAS 设置；本地 CSV 开关可独立修改")
		}
	}
	// New secret first, then atomically point configuration at it. Failed saves
	// leave both the running worker and the previous secret untouched.
	if token != "" {
		f, e := os.CreateTemp(app.dir, "upload-token-")
		if e != nil {
			return e
		}
		path := f.Name()
		f.Close()
		if e = writePrivate(path, []byte(token+"\n")); e != nil {
			os.Remove(path)
			return e
		}
		cfg.TokenFile = path
		defer func() {
			if err != nil {
				os.Remove(path)
			}
		}()
	}
	data, e := json.MarshalIndent(cfg, "", "  ")
	if e != nil {
		err = e
		return err
	}
	err = writePrivate(filepath.Join(app.dir, "event-upload.json"), append(data, '\n'))
	if err != nil {
		return err
	}
	if changed {
		if app.uploadCancel != nil {
			app.uploadCancel()
			app.uploadCancel = nil
		}
		if app.eventUpload != nil {
			app.eventUpload.client.CloseIdleConnections()
		}
		app.eventUpload = next
		if next != nil {
			next.cfg = cfg
			ctx, cancel := context.WithCancel(context.Background())
			app.uploadCancel = cancel
			go next.run(ctx)
		}
	}
	if app.keylogs != nil {
		app.keylogs.setEnabled(cfg.TLSKeylog)
	}
	if *cfg.LocalCSV {
		if app.requestLogs == nil {
			app.requestLogs = &requestLog{dir: filepath.Join(app.dir, "request-logs"), limit: 100 * 1024 * 1024}
		}
	} else {
		app.requestLogs = nil
	}
	if token != "" && filepath.Dir(old.TokenFile) == app.dir && strings.HasPrefix(filepath.Base(old.TokenFile), "upload-token-") {
		_ = os.Remove(old.TokenFile)
	}
	return nil
}

func (app *application) testLogging(input loggingInput) error {
	app.loggingMu.RLock()
	old, err := readLoggingSettings(app.dir)
	app.loggingMu.RUnlock()
	if err != nil {
		return err
	}
	cfg := input.uploadSettings
	cfg.TokenFile = old.TokenFile
	u, err := newEventUploader(cfg, input.Token)
	if err != nil {
		return err
	}
	defer u.client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// Invalid event authenticates the request without creating a synthetic DB row.
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.Endpoint, strings.NewReader("{}\n"))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := u.client.Do(req)
	if err != nil {
		return errors.New("HTTPS 连接失败，请检查地址、证书、DNS 和网络")
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode != 400 || !bytes.Contains(data, []byte("invalid_event")) {
		return errors.New("接收端验证失败，请检查地址、令牌及接收服务状态")
	}
	return nil
}
func (app *application) loggingAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			http.Error(w, "use GET, PUT or POST", 405)
			return
		}
		var input loggingInput
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		d.DisallowUnknownFields()
		if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
			http.Error(w, "无效的日志配置", 400)
			return
		}
		var err error
		if r.Method == http.MethodPost {
			err = app.testLogging(input)
		} else {
			err = app.saveLogging(input)
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if r.Method == http.MethodPost {
			responseJSON(w, 200, map[string]any{"ok": true, "message": "HTTPS 与令牌验证通过（未写入数据库测试记录，不代表数据库健康检查）"})
			return
		}
	}
	state, err := app.loggingSnapshot()
	if err != nil {
		http.Error(w, "无法读取日志配置", 500)
		return
	}
	responseJSON(w, 200, state)
}

// A pipe feeds the CSV writer while the uploader consumes the original body.
// CSV errors drain the pipe so a full disk cannot truncate the NAS record.
type csvTee struct {
	source io.Reader
	target *io.PipeWriter
}

func (t *csvTee) Read(p []byte) (int, error) {
	n, err := t.source.Read(p)
	if n > 0 {
		_, _ = t.target.Write(p[:n])
	}
	if err != nil {
		_ = t.target.CloseWithError(err)
	}
	return n, err
}
func (app *application) recordRequest(req *http.Request) {
	app.loggingMu.RLock()
	defer app.loggingMu.RUnlock()
	u, l := app.eventUpload, app.requestLogs
	report := func(err error) {
		if err != nil && app.monitor != nil {
			app.monitor.message("HTTPS JSON 请求 CSV 保存失败: " + err.Error())
		}
	}
	if u != nil && l != nil && req.Body != nil {
		reader, writer := io.Pipe()
		copyReq := req.Clone(req.Context())
		copyReq.Body = reader
		done := make(chan error, 1)
		go func() { err := l.record(copyReq); io.Copy(io.Discard, reader); reader.Close(); done <- err }()
		original := req.Body
		req.Body = io.NopCloser(&csvTee{source: original, target: writer})
		u.record(req)
		writer.Close()
		req.Body = original
		report(<-done)
	} else {
		if u != nil {
			u.record(req)
		}
		if l != nil {
			report(l.record(req))
		}
	}
}
