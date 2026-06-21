// Package tunnel implements the CDN-compatible Meek-style tunnel transport.
//
// Protocol:
//
//	POST /tunnel  X-Naive-Session:ID  X-Naive-Target:host:port (first POST only)
//	  body    = upstream data (may be empty)
//	  200 OK  body = downstream data (target responded within poll timeout)
//	  204     body empty (no downstream data this poll, try again)
//	  410     target closed connection
//
// Each POST is a complete request/response cycle. CDNs that buffer streaming
// GET responses handle these correctly because each response is short and closed.
package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config holds the proxy connection parameters.
type Config struct {
	ProxyURL           string
	TunnelPath         string
	TLSFingerprint     string // unused, kept for config compat
	InsecureSkipVerify bool
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func dialTLS(ctx context.Context, cfg Config) (net.Conn, error) {
	u, err := url.Parse(cfg.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", host, err)
	}
	tlsCfg := &tls.Config{
		ServerName:         host,
		NextProtos:         []string{"http/1.1"},
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
	tlsConn := tls.Client(rawConn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	log.Printf("[tunnel] connected %s ALPN=%s", net.JoinHostPort(host, port), tlsConn.ConnectionState().NegotiatedProtocol)
	return tlsConn, nil
}

func buildAuthHeader(cfg Config) string {
	u, _ := url.Parse(cfg.ProxyURL)
	if u == nil || u.User == nil {
		return ""
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func tunnelPath(cfg Config) string {
	if cfg.TunnelPath != "" {
		return cfg.TunnelPath
	}
	return "/tunnel"
}

func proxyHost(cfg Config) string {
	u, _ := url.Parse(cfg.ProxyURL)
	if u == nil {
		return ""
	}
	return u.Host
}

// isTimeout returns true if err is a network deadline/timeout error.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Stream establishes a Meek-style tunnel and copies data bidirectionally.
func Stream(ctx context.Context, cfg Config, target string, appConn net.Conn) error {
	sessionID := newSessionID()
	auth := buildAuthHeader(cfg)
	path := tunnelPath(cfg)
	host := proxyHost(cfg)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// upCh buffers upstream chunks from the app (non-blocking producer).
	upCh := make(chan []byte, 32)

	// Reader goroutine: continuously drain appConn into upCh.
	go func() {
		defer cancel()
		buf := make([]byte, 16*1024)
		for {
			n, err := appConn.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case upCh <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	isFirst := true

	// Give the app a moment to send initial data before the first POST.
	// This avoids creating an empty-body first POST that wastes a poll cycle.
	var initData []byte
	select {
	case chunk := <-upCh:
		initData = chunk
	case <-time.After(100 * time.Millisecond):
		// no initial data; first POST will create the session with empty body
	case <-ctx.Done():
		return nil
	}

	conn, err := dialTLS(ctx, cfg)
	if err != nil {
		return err
	}

	var mu sync.Mutex // protect conn

	// Poll loop: each iteration sends one POST and writes downstream to appConn.
	for {
		if ctx.Err() != nil {
			mu.Lock()
			conn.Close()
			mu.Unlock()
			return nil
		}

		// Collect all pending upstream data (non-blocking drain of upCh).
		upData := initData
		initData = nil
	drain:
		for {
			select {
			case chunk := <-upCh:
				upData = append(upData, chunk...)
			default:
				break drain
			}
		}

		down, closed, err := meekPost(ctx, cfg, host, path, auth, sessionID, target, upData, isFirst, conn)
		isFirst = false

		if err != nil {
			log.Printf("[tunnel] reconnect: %v", err)
			mu.Lock()
			conn.Close()
			mu.Unlock()
			// Reconnect the CDN TLS connection; server session stays alive.
			conn, err = dialTLS(ctx, cfg)
			if err != nil {
				return err
			}
			continue
		}

		if len(down) > 0 {
			if _, werr := appConn.Write(down); werr != nil {
				conn.Close()
				return nil
			}
			log.Printf("[tunnel] downstream %d bytes", len(down))
		}

		if closed {
			log.Printf("[tunnel] session closed by target")
			conn.Close()
			return nil
		}

		// If no data in either direction, add a small back-off to avoid tight loop.
		if len(down) == 0 && len(upData) == 0 {
			select {
			case <-ctx.Done():
				conn.Close()
				return nil
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

// meekPost sends one POST /tunnel using an existing persistent TLS conn.
// The caller must not use conn concurrently.
func meekPost(ctx context.Context, cfg Config, host, path, auth, sessionID, target string, upData []byte, isFirst bool, conn net.Conn) (down []byte, closed bool, err error) {
	_ = cfg // reserved

	var sb strings.Builder
	sb.WriteString("POST ")
	sb.WriteString(path)
	sb.WriteString(" HTTP/1.1\r\nHost: ")
	sb.WriteString(host)
	sb.WriteString("\r\nX-Naive-Session: ")
	sb.WriteString(sessionID)
	if isFirst && target != "" {
		sb.WriteString("\r\nX-Naive-Target: ")
		sb.WriteString(target)
	}
	sb.WriteString(fmt.Sprintf("\r\nContent-Length: %d\r\nContent-Type: application/octet-stream\r\nConnection: keep-alive\r\n", len(upData)))
	if auth != "" {
		sb.WriteString("Proxy-Authorization: ")
		sb.WriteString(auth)
		sb.WriteString("\r\nX-Naive-Auth: ")
		sb.WriteString(auth)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")

	if _, err := io.WriteString(conn, sb.String()); err != nil {
		return nil, false, fmt.Errorf("write headers: %w", err)
	}
	if len(upData) > 0 {
		if _, err := conn.Write(upData); err != nil {
			return nil, false, fmt.Errorf("write body: %w", err)
		}
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, false, fmt.Errorf("read response: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("[tunnel] POST %d up → %d", len(upData), resp.StatusCode)

	sessionClosed := resp.Header.Get("X-Naive-Closed") == "1"

	switch resp.StatusCode {
	case http.StatusOK:
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("read downstream: %w", err)
		}
		return data, sessionClosed, nil
	case http.StatusNoContent:
		return nil, sessionClosed, nil
	case http.StatusGone:
		return nil, true, nil
	default:
		return nil, true, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}
