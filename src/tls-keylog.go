package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func tlsKeylogAllowed(cfg *runtimeConfig, remote string) bool {
	addr, err := netip.ParseAddrPort(remote)
	return err == nil && !cfg.MonitorOnly && cfg.Scope.selected(addr.Addr())
}

const keylogLimit int64 = 8 << 20
const keylogSegment int64 = 1 << 20

// Only this directory holds session secrets; never send their contents to syslog.
type tlsKeylog struct {
	mu                                sync.Mutex
	dir                               string
	enabled                           bool
	active                            string
	started                           time.Time
	size, used                        int64
	saved, dropped, uploaded, retries uint64
	lastError                         string
}

func newTLSKeylog(dir string) *tlsKeylog {
	k := &tlsKeylog{dir: dir}
	if err := os.MkdirAll(dir, 0700); err != nil {
		k.lastError = "无法创建内存密钥日志目录"
		return k
	}
	_ = os.Chmod(dir, 0700)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if strings.HasSuffix(p, ".open") {
			if os.Rename(p, strings.TrimSuffix(p, ".open")+".keys") != nil {
				continue
			}
			p = strings.TrimSuffix(p, ".open") + ".keys"
		}
		if strings.HasSuffix(p, ".keys") {
			if s, err := os.Stat(p); err == nil {
				k.used += s.Size()
				_ = os.Chmod(p, 0600)
			}
		}
	}
	return k
}
func (k *tlsKeylog) rotate() { // caller holds mu
	if k.active == "" {
		return
	}
	if err := os.Rename(k.active, strings.TrimSuffix(k.active, ".open")+".keys"); err != nil {
		k.lastError = "密钥日志轮转失败"
		return
	}
	k.active = ""
	k.size = 0
}
func (k *tlsKeylog) setEnabled(on bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.enabled = on
	if !on {
		k.rotate()
	}
}
func (k *tlsKeylog) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.enabled {
		return len(p), nil
	}
	if k.used+int64(len(p)) > keylogLimit {
		k.dropped++
		k.lastError = "内存缓存已满，丢弃新密钥记录"
		return len(p), nil
	}
	if k.size+int64(len(p)) > keylogSegment {
		k.rotate()
	}
	if k.active == "" {
		host, _ := os.Hostname()
		host = regexp.MustCompile(`[^a-zA-Z0-9_-]`).ReplaceAllString(host, "_")
		f, err := os.CreateTemp(k.dir, host+"-"+strconv.Itoa(os.Getpid())+"-"+time.Now().Format("20060102-150405")+"-*.open")
		if err != nil {
			k.dropped++
			k.lastError = "无法创建密钥日志"
			return len(p), nil
		}
		k.active = f.Name()
		f.Close()
		k.started = time.Now()
	}
	f, err := os.OpenFile(k.active, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		k.dropped++
		k.lastError = "无法打开密钥日志"
		return len(p), nil
	}
	n, err := f.Write(p)
	f.Close()
	k.size += int64(n)
	k.used += int64(n)
	if err != nil || n != len(p) {
		k.dropped++
		k.lastError = "密钥日志写入失败"
	} else {
		k.saved++
	}
	if k.size >= keylogSegment {
		k.rotate()
	}
	// A debug logging failure must never break TLS.
	return len(p), nil
}
func (k *tlsKeylog) status() map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()
	return map[string]any{"enabled": k.enabled, "buffer_bytes": k.used, "buffer_limit": keylogLimit, "saved": k.saved, "dropped": k.dropped, "uploaded": k.uploaded, "retries": k.retries, "last_error": k.lastError}
}
func (k *tlsKeylog) rotateDue() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.active != "" && time.Since(k.started) >= time.Minute {
		k.rotate()
	}
}
func (k *tlsKeylog) run(ctx context.Context) {
	// Rotation must continue even while an unavailable NAS stalls an upload.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				k.rotateDue()
			}
		}
	}()

	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			k.uploadPending(ctx, uploadTLSKeys)
		}
	}
}

func (k *tlsKeylog) uploadPending(ctx context.Context, upload func(context.Context, string, int64) error) {
	files, _ := filepath.Glob(filepath.Join(k.dir, "*.keys"))
	for _, p := range files {
		if ctx.Err() != nil {
			return
		}
		s, err := os.Stat(p)
		if err != nil {
			continue
		}
		err = upload(ctx, p, s.Size())
		k.mu.Lock()
		if err != nil {
			k.retries++
			k.lastError = err.Error()
		} else if os.Remove(p) == nil {
			k.used -= s.Size()
			k.uploaded++
			k.lastError = ""
		} else {
			k.lastError = "上传成功但本地文件删除失败"
		}
		k.mu.Unlock()
		if err != nil {
			break
		}
	}
}

var sftpSafe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func uploadTLSKeys(ctx context.Context, path string, size int64) error {
	var c struct {
		Host string
		Port int
		User string
	}
	b, err := os.ReadFile("/etc/packet-capture/config.json")
	if err != nil || json.Unmarshal(b, &c) != nil || !sftpSafe.MatchString(c.Host) || !sftpSafe.MatchString(c.User) || c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("独立抓包服务 SFTP 配置不可用")
	}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	name := filepath.Base(path)
	cmd := exec.CommandContext(ctx, "sftp", "-S", "/usr/libexec/packet-capture/ssh-client", "-P", strconv.Itoa(c.Port), "-b", "-", c.User+"@"+c.Host)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = strings.NewReader(fmt.Sprintf("cd /tls-keylogs\nput %s %s.part\nrename %s.part %s\nls -ln %s\n", path, name, name, name, name))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("SFTP 密钥日志上传失败，请检查连接、权限及 /tls-keylogs 映射")
	}
	for _, line := range strings.Split(string(output), "\n") {
		f := strings.Fields(line)
		if len(f) >= 6 && strings.HasPrefix(f[0], "-") && (f[len(f)-1] == name || f[len(f)-1] == "./"+name) {
			n, e := strconv.ParseInt(f[4], 10, 64)
			if e == nil && n == size {
				return nil
			}
		}
	}
	return fmt.Errorf("SFTP 未确认远端文件大小，保留本地密钥日志")
}

// ServeTLS adjusts a clone of the server configuration, not the original.
// Explicit ALPN is required when returning our own per-client configuration.
func keylogTLSConfig(base *tls.Config, cfg *runtimeConfig, remote string, keys *tlsKeylog) *tls.Config {
	next := base.Clone()
	next.GetConfigForClient = nil
	next.NextProtos = []string{"h2", "http/1.1"}
	if tlsKeylogAllowed(cfg, remote) {
		next.KeyLogWriter = keys
	}
	return next
}
