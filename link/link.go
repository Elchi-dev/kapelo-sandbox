// Package link is the one connection between a sandbox and the control
// plane. The sandbox dials out over TLS with its client certificate and asks
// to upgrade the HTTP request to kapelo-mux/1; from then on the connection
// carries yamux streams in both directions. Each side serves plain HTTP on
// the streams the other side opens, so commands, tool calls and the
// preview tunnel are ordinary requests on one socket. No port is
// opened on the sandbox's machine, and it works behind any NAT.
package link

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"time"

	"github.com/hashicorp/yamux"
)

const Protocol = "kapelo-mux/1"

func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 20 * time.Second
	c.ConnectionWriteTimeout = 30 * time.Second
	c.LogOutput = io.Discard
	return c
}

// Upgrade turns an accepted HTTP request into the server side of a link.
func Upgrade(w http.ResponseWriter, r *http.Request) (*yamux.Session, error) {
	if r.Header.Get("Upgrade") != Protocol {
		http.Error(w, "expected Upgrade: "+Protocol, http.StatusUpgradeRequired)
		return nil, errors.New("link: no upgrade header")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be upgraded", http.StatusInternalServerError)
		return nil, errors.New("link: response cannot be hijacked")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("link: hijack: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + Protocol + "\r\n\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return yamux.Server(&bufferedConn{Conn: conn, r: rw.Reader}, muxConfig())
}

// Dial opens the client side of a link to https://addr/agent/connect.
func Dial(ctx context.Context, addr string, cfg *tls.Config) (*yamux.Session, error) {
	d := tls.Dialer{Config: cfg, NetDialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("link: dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/agent/connect", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", Protocol)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("link: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("link: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		conn.Close()
		return nil, fmt.Errorf("link: control plane answered %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	_ = conn.SetDeadline(time.Time{})
	return yamux.Client(&bufferedConn{Conn: conn, r: br}, muxConfig())
}

// bufferedConn keeps bytes the HTTP reader already buffered.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Serve answers HTTP requests on streams the other side opens, until the
// session ends.
func Serve(s *yamux.Session, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second, ErrorLog: stdlog.New(io.Discard, "", 0)}
	err := srv.Serve(s)
	if s.IsClosed() {
		return nil
	}
	return err
}

// Client makes HTTP requests on new streams of the session. The host in
// request URLs is ignored; use http://link/....
func Client(s *yamux.Session) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return s.Open()
		},
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}}
}
