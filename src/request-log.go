package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Requests append under one lock. An on-disk checkpoint allows an interrupted
// append to be rolled back without losing earlier complete rows.
type requestLog struct {
	mu            sync.Mutex
	dir           string
	limit         int64
	rotateBytes   int64
	saved, failed uint64
	lastError     string
}

func (l *requestLog) record(req *http.Request) (err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer func() {
		if err != nil {
			l.failed++
			l.lastError = err.Error()
		} else {
			l.saved++
			l.lastError = ""
		}
	}()
	if err = os.MkdirAll(l.dir, 0700); err != nil {
		return err
	}
	if err = l.recoverAppend(); err != nil {
		return err
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	type oldFile struct {
		name string
		size int64
	}
	var old []oldFile
	var used int64
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "checkpoint-") && strings.HasSuffix(entry.Name(), ".tmp") {
			if err = os.Remove(filepath.Join(l.dir, entry.Name())); err != nil {
				return err
			}
			continue
		}

		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "https-") {
			name := filepath.Join(l.dir, entry.Name())
			if strings.HasSuffix(name, ".part") {
				if err = os.Remove(name); err != nil {
					return err
				}
				continue
			}
			if !strings.HasSuffix(name, ".csv") {
				continue
			}
			info, e := entry.Info()
			if e != nil {
				return e
			}
			old = append(old, oldFile{name, info.Size()})
			used += info.Size()
		}
	}
	sort.Slice(old, func(i, j int) bool { return old[i].name < old[j].name })
	rotate := l.rotateBytes
	if rotate <= 0 {
		rotate = 5 * 1024 * 1024
	}
	if rotate > l.limit/4 {
		rotate = l.limit / 4
	}
	var file *os.File
	var originalSize int64
	// Keep appending to the newest file, including after service restarts.
	if len(old) > 0 && old[len(old)-1].size < rotate {
		latest := old[len(old)-1]
		originalSize = latest.size
		file, err = os.OpenFile(latest.name, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		checkpoint := appendCheckpoint{File: filepath.Base(latest.name), Size: originalSize}
		if err = l.saveCheckpoint(checkpoint); err != nil {
			file.Close()
			return err
		}
		old = old[:len(old)-1]
		used -= originalSize
	} else {
		file, err = os.CreateTemp(l.dir, "https-"+systemNow().Format("20060102-150405.000000000")+"-*.part")
		if err != nil {
			return err
		}
	}
	committed := false
	defer func() {
		if err != nil && !committed {
			if originalSize == 0 {
				file.Close()
				os.Remove(file.Name())
			} else {
				// Keep the checkpoint on rollback failure so the next attempt retries.
				rollback := file.Truncate(originalSize)
				if rollback == nil {
					rollback = file.Sync()
				}
				file.Close()
				if rollback == nil {
					rollback = os.Remove(l.checkpointPath())
				}
				err = errors.Join(err, rollback)
			}
		} else {
			file.Close()
		}
	}()
	var written int64
	sink := &requestLogWriter{write: func(p []byte) (int, error) {
		if originalSize+written+int64(len(p)) > l.limit {
			return 0, errors.New("完整请求记录超过本地 CSV 总容量上限")
		}
		for (used+originalSize+written+int64(len(p)) > l.limit || len(old) >= 10000) && len(old) > 0 {
			if e := os.Remove(old[0].name); e != nil {
				return 0, e
			}
			used -= old[0].size
			old = old[1:]
		}
		n, e := file.Write(p)
		written += int64(n)
		return n, e
	}}
	buffer := bufio.NewWriterSize(sink, 32768)
	if originalSize == 0 {
		header := csv.NewWriter(buffer)
		if err = header.Write([]string{"time", "source_ip", "request_domain", "request_path", "request_body"}); err != nil {
			return err
		}
		header.Flush()
		if err = header.Error(); err != nil {
			return err
		}
	}
	source := req.RemoteAddr
	if host, _, e := net.SplitHostPort(source); e == nil {
		source = host
	}
	domain := req.Host
	if host, _, e := net.SplitHostPort(domain); e == nil {
		domain = host
	}
	fields := []string{systemNow().Format(displayTimeLayout), source, domain, req.URL.RequestURI()}
	// Always quote fields and stream quote escaping, without a body-size truncation.
	for _, value := range fields {
		if _, err = fmt.Fprintf(buffer, "\"%s\",", strings.ReplaceAll(value, "\"", "\"\"")); err != nil {
			return err
		}
	}
	if err = buffer.WriteByte('"'); err != nil {
		return err
	}
	if req.Body != nil {
		chunk := make([]byte, 32768)
		for {
			n, e := req.Body.Read(chunk)
			if n > 0 {
				if _, err = buffer.WriteString(strings.ReplaceAll(string(chunk[:n]), "\"", "\"\"")); err != nil {
					return err
				}
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
		}
	}
	if _, err = buffer.WriteString("\"\n"); err != nil {
		return err
	}
	if err = buffer.Flush(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if originalSize == 0 {
		if err = os.Rename(file.Name(), strings.TrimSuffix(file.Name(), ".part")+".csv"); err != nil {
			return err
		}
		committed = true
		return l.syncDirectory()
	}
	if err = os.Remove(l.checkpointPath()); err != nil {
		return err
	}
	committed = true
	return l.syncDirectory()
}

type requestLogWriter struct{ write func([]byte) (int, error) }

func (w *requestLogWriter) Write(p []byte) (int, error) { return w.write(p) }

// The checkpoint is persisted before any bytes are appended. A leftover
// checkpoint means that the last row was not committed and must be discarded.
type appendCheckpoint struct {
	File string
	Size int64
}

func (l *requestLog) checkpointPath() string { return filepath.Join(l.dir, "append-checkpoint.json") }
func (l *requestLog) saveCheckpoint(c appendCheckpoint) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(l.dir, "checkpoint-*.tmp")
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), l.checkpointPath()); err != nil {
		return err
	}
	return l.syncDirectory()
}
func (l *requestLog) syncDirectory() error {
	f, err := os.Open(l.dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (l *requestLog) recoverAppend() error {
	data, err := os.ReadFile(l.checkpointPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var c appendCheckpoint
	if err = json.Unmarshal(data, &c); err != nil {
		return err
	}
	if c.Size < 0 || filepath.Base(c.File) != c.File || !strings.HasPrefix(c.File, "https-") || !strings.HasSuffix(c.File, ".csv") {
		return errors.New("invalid CSV append checkpoint")
	}
	f, err := os.OpenFile(filepath.Join(l.dir, c.File), os.O_WRONLY, 0600)
	if os.IsNotExist(err) {
		return os.Remove(l.checkpointPath())
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < c.Size {
		return errors.New("CSV smaller than append checkpoint")
	}
	if err = f.Truncate(c.Size); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return os.Remove(l.checkpointPath())
}
