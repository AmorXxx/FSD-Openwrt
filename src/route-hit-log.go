package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Quote client-controlled values so control characters cannot forge log lines.
// Body fragments carry the same request ID as this metadata line.
func routeHitMessage(req *http.Request, rule, action string) string {
	sni := ""
	if req.TLS != nil {
		sni = req.TLS.ServerName
	}
	return fmt.Sprintf("JSON 路由命中 · 请求=%s · 客户端=%q · 方法=%q · 域名=%q · 路径=%q · 协议=%q · SNI=%q · 规则=%q · 动作=%s",
		requestLogID(req), req.RemoteAddr, req.Method, req.Host, req.URL.RequestURI(), req.Proto, sni, rule, action)
}

type requestLogIDKey struct{}

func requestLogID(req *http.Request) string {
	id, _ := req.Context().Value(requestLogIDKey{}).(string)
	if id == "" {
		return "—"
	}
	return id
}

const bodyLogLimit int64 = 4 * 1024

type bodyLogReader struct {
	io.ReadCloser
	emit                 func(string)
	id                   string
	parts, total, logged int64
	done                 bool
	err                  error
}

func (b *bodyLogReader) Read(p []byte) (int, error) {
	// Worst-case escaping remains below the usual syslog line-size limit.
	if b.logged < bodyLogLimit && len(p) > 128 {
		p = p[:128]
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.total += int64(n)
		count := min(int64(n), bodyLogLimit-b.logged)
		if count > 0 {
			b.parts++
			b.logged += count
			b.emit(fmt.Sprintf("JSON 请求体 · 请求=%s · 分段=%d · 内容=%q", b.id, b.parts, string(p[:count])))
		}
	}
	if err != nil {
		b.done = true
		if err != io.EOF {
			b.err = err
		}
	}
	return n, err
}
func (app *application) traceRequestBody(req *http.Request) (*http.Request, func()) {
	id := randomToken()[:16]
	req = req.WithContext(context.WithValue(req.Context(), requestLogIDKey{}, id))
	b := &bodyLogReader{ReadCloser: req.Body, emit: app.monitor.message, id: id}
	if req.Body == nil {
		b.done = true
	} else {
		req.Body = b
	}
	return req, func() {
		// Also record bodies when both CSV and database recording are disabled.
		if !b.done {
			_, _ = io.Copy(io.Discard, b)
		}
		state := "完整"
		if b.err != nil {
			state = fmt.Sprintf("不完整，读取错误=%q", b.err.Error())
		}
		preview := "未截断"
		if b.total > b.logged {
			preview = "已截断，仅打印前 4096 字节"
		}
		app.monitor.message(fmt.Sprintf("JSON 请求体结束 · 请求=%s · 已读取字节数=%d · 已打印字节数=%d · 分段数=%d · 读取状态=%s · 日志=%s", id, b.total, b.logged, b.parts, state, preview))
	}
}
