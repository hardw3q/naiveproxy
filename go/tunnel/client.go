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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Config holds the proxy connection parameters.
type Config struct {
	ProxyURL           string
	TunnelPath         string
	TLSFingerprint     string
	InsecureSkipVerify bool
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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

func makeHTTPClient(cfg Config) *http.Client {
	u, _ := url.Parse(cfg.ProxyURL)
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	serverAddr := net.JoinHostPort(host, port)

	var helloID utls.ClientHelloID
	switch cfg.TLSFingerprint {
	case "firefox", "firefox_auto":
		helloID = utls.HelloFirefox_Auto
	case "safari", "ios":
		helloID = utls.HelloIOS_Auto
	default:
		helloID = utls.HelloChrome_Auto
	}

	transport := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string, _ *tls.Config) (net.Conn, error) {
			rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", serverAddr)
			if err != nil {
				return nil, fmt.Errorf("dial %s: %w", host, err)
			}
			tlsCfg := &utls.Config{
				ServerName:         host,
				InsecureSkipVerify: cfg.InsecureSkipVerify,
			}
			tlsConn := utls.UClient(rawConn, tlsCfg, helloID)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				rawConn.Close()
				return nil, fmt.Errorf("TLS handshake: %w", err)
			}
			log.Printf("[tunnel] connected %s ALPN=%s", serverAddr, tlsConn.ConnectionState().NegotiatedProtocol)
			return tlsConn, nil
		},
	}

	return &http.Client{Transport: transport}
}

// Stream establishes a Meek-style tunnel and copies data bidirectionally.
func Stream(ctx context.Context, cfg Config, target string, appConn net.Conn) error {
	sessionID := newSessionID()
	auth := buildAuthHeader(cfg)

	u, _ := url.Parse(cfg.ProxyURL)
	baseURL := "https://" + u.Host + tunnelPath(cfg)

	client := makeHTTPClient(cfg)

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
	var initData []byte
	select {
	case chunk := <-upCh:
		initData = chunk
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		return nil
	}

	// Poll loop: each iteration sends one POST and writes downstream to appConn.
	for {
		if ctx.Err() != nil {
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

		down, closed, err := meekPost(ctx, client, baseURL, auth, sessionID, target, upData, isFirst)
		isFirst = false

		if err != nil {
			log.Printf("[tunnel] post error: %v", err)
			// On error the http.Client will create a new connection on next request.
			// Small back-off before retry.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}

		if len(down) > 0 {
			if _, werr := appConn.Write(down); werr != nil {
				return nil
			}
			log.Printf("[tunnel] downstream %d bytes", len(down))
		}

		if closed {
			log.Printf("[tunnel] session closed by target")
			return nil
		}

		// If no data in either direction, add a small back-off to avoid tight loop.
		if len(down) == 0 && len(upData) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

// meekPost sends one POST /tunnel using the http.Client (supports HTTP/1.1 and HTTP/2).
func meekPost(ctx context.Context, client *http.Client, baseURL, auth, sessionID, target string, upData []byte, isFirst bool) (down []byte, closed bool, err error) {
	var body io.Reader
	if len(upData) > 0 {
		body = bytes.NewReader(upData)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, body)
	if err != nil {
		return nil, false, err
	}

	req.Header.Set("X-Naive-Session", sessionID)
	if isFirst && target != "" {
		req.Header.Set("X-Naive-Target", target)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(upData))
	if auth != "" {
		req.Header.Set("Proxy-Authorization", auth)
		req.Header.Set("X-Naive-Auth", auth)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("do request: %w", err)
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
		body, _ := io.ReadAll(resp.Body)
		return nil, true, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}
