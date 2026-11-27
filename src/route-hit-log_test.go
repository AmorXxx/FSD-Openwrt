package main

import (
	"crypto/tls"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestRouteHitLogActualPathAndSafeFields(t *testing.T) {
	req := httptest.NewRequest("POST", "https://example.com/a/b?x=1%202", strings.NewReader("private-body"))
	req.RemoteAddr = "[2001:db8::1]:12345"
	req.TLS = &tls.ConnectionState{ServerName: "example.com"}
	req.Header.Set("Authorization", "Bearer private-token")
	message := routeHitMessage(req, "*", "重置 HTTP/2 当前流")
	for _, want := range []string{`客户端="[2001:db8::1]:12345"`, `方法="POST"`, `域名="example.com"`, `路径="/a/b?x=1%202"`, `SNI="example.com"`, `规则="*"`, "重置 HTTP/2 当前流"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %s: %s", want, message)
		}
	}
	if strings.Contains(message, "private-") {
		t.Fatal("body or authorization leaked")
	}
	req.Host = "example.com\nforged\r\tline"
	message = routeHitMessage(req, "*", "关闭连接")
	if strings.ContainsAny(message, "\n\r\t") {
		t.Fatal("unescaped log control characters")
	}
}

func TestBodyLogPreservesBytesAndReportsFailure(t *testing.T) {
	data := strings.Repeat("中文\n\x00", 100)
	var lines []string
	b := &bodyLogReader{ReadCloser: io.NopCloser(strings.NewReader(data)), id: "test", emit: func(s string) { lines = append(lines, s) }}
	got, err := io.ReadAll(b)
	if err != nil || string(got) != data || b.total != int64(len(data)) || !b.done {
		t.Fatal("body changed")
	}
	var rebuilt strings.Builder
	for _, line := range lines {
		if len(line) > 1024 || strings.Contains(line, "\n") {
			t.Fatal("unsafe log line")
		}
		quoted := strings.SplitN(line, "内容=", 2)[1]
		part, e := strconv.Unquote(quoted)
		if e != nil {
			t.Fatal(e)
		}
		rebuilt.WriteString(part)
	}
	if rebuilt.String() != data {
		t.Fatal("log truncated body")
	}
	b = &bodyLogReader{ReadCloser: io.NopCloser(errorBody{}), emit: func(string) {}}
	_, err = io.ReadAll(b)
	if err == nil || b.err == nil || !b.done {
		t.Fatal("read error lost")
	}
}

type errorBody struct{}

func (errorBody) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestBodyLogLimitDoesNotTruncateRequest(t *testing.T) {
	for _, size := range []int{4095, 4096, 4097, 100000} {
		data := strings.Repeat("x", size)
		var printed strings.Builder
		b := &bodyLogReader{ReadCloser: io.NopCloser(strings.NewReader(data)), emit: func(line string) {
			part, err := strconv.Unquote(strings.SplitN(line, "内容=", 2)[1])
			if err != nil {
				t.Fatal(err)
			}
			printed.WriteString(part)
		}}
		got, err := io.ReadAll(b)
		if err != nil || string(got) != data || b.total != int64(size) {
			t.Fatalf("downstream body truncated at size %d", size)
		}
		expected := min(size, int(bodyLogLimit))
		if printed.Len() != expected || b.logged != int64(expected) || printed.String() != data[:expected] {
			t.Fatalf("incorrect log prefix at size %d", size)
		}
	}
}
