package main

import (
	"context"
	"net"
	"net/http"
)

type requestConnectionKey struct{}

func requestConnectionContext(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, requestConnectionKey{}, conn)
}

// Abort only the current HTTP/2 stream; HTTP/1.1 closes the connection.
// ErrAbortHandler makes net/http emit RST_STREAM without an HTTP response.
func closeRequestConnection(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 2 {
		panic(http.ErrAbortHandler)
	}
	if conn, ok := r.Context().Value(requestConnectionKey{}).(net.Conn); ok {
		_ = conn.Close()
	} else if hijacker, ok := w.(http.Hijacker); ok {
		if conn, _, err := hijacker.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
	// Prevent net/http from implicitly emitting a 200 response on handler return.
	panic(http.ErrAbortHandler)
}
