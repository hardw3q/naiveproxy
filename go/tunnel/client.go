// Package tunnel implements the CDN-compatible split tunnel transport.
//
// Protocol (two connections, works despite CDN request-body buffering):
//
//	GET /tunnel  X-Naive-Session:ID X-Naive-Target:host:port
//	  → server dials target, streams target→client in response body (CDNs never buffer responses)
//
//	POST /tunnel X-Naive-Session:ID  body=chunk
//	  → server writes chunk to target (small complete body, CDN buffers quickly, then forwards)
//
// Upstream (client→server) sends sequential chunks; each POST waits for 200 OK
// before the next. Downstream (server→client) is continuous streaming via GET response.
package tunnel

import (
	"bufio"
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

	utls "github.com/refraction-networking/utls"
)

// Config holds the proxy connection parameters.
type Config struct {
	// ProxyURL is the full HTTPS URL of the proxy, including credentials.
	// Example: "https://user:pass@cdn.example.com"
	ProxyURL string

	// TunnelPath is the endpoint path. Defaults to "/tunnel".
	TunnelPath string

	// TLSFingerprint selects the utls ClientHello preset.
	// Supported: "chrome_auto" (default), "chrome_120", "firefox_120".
	TLSFingerprint string

	// InsecureSkipVerify disables server certificate verification (for testing only).
	InsecureSkipVerify bool
}

func chooseFingerprint(preset string) utls.ClientHelloID {
	switch strings.ToLower(preset) {
	case "chrome_120":
		return utls.HelloChrome_120
	case "firefox_120":
		return utls.HelloFirefox_120
	default:
		return utls.HelloChrome_Auto
	}
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
	log.Printf("[debug] TCP connected to %s", net.JoinHostPort(host, port))

	// For CDN-mediated connections the TLS fingerprint is invisible (CDN terminates TLS),
	// so use standard crypto/tls which correctly honours NextProtos. utls Chrome preset
	// overrides NextProtos and would negotiate h2, breaking our raw HTTP/1.1 framing.
	tlsCfg := &tls.Config{
		ServerName:         host,
		NextProtos:         []string{"http/1.1"},
		Renegotiation:      tls.RenegotiateFreelyAsClient,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
	tlsConn := tls.Client(rawConn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	log.Printf("[debug] TLS handshake OK, ALPN=%s", tlsConn.ConnectionState().NegotiatedProtocol)
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

// Stream establishes a split POST/GET tunnel to the proxy and copies data
// bidirectionally between appConn and the remote target.
func Stream(ctx context.Context, cfg Config, target string, appConn net.Conn) error {
	sessionID := newSessionID()
	auth := buildAuthHeader(cfg)
	path := tunnelPath(cfg)
	host := proxyHost(cfg)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// — Downstream: GET /tunnel → server streams target→client in response body.
	downConn, downResp, err := connectDown(ctx, cfg, host, path, auth, target, sessionID)
	if err != nil {
		return fmt.Errorf("tunnel down: %w", err)
	}
	defer downConn.Close()
	defer downResp.Body.Close()

	if downResp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy GET returned %d", downResp.StatusCode)
	}

	// — Upstream: read from appConn, POST chunks to server.
	go func() {
		defer cancel()
		sendUpstream(ctx, cfg, host, path, auth, sessionID, appConn)
	}()

	// Copy server response (downstream) → appConn.
	io.Copy(appConn, downResp.Body) //nolint:errcheck
	return nil
}

// connectDown opens a GET /tunnel request and returns when the server sends 200 OK.
// The caller reads downResp.Body to receive downstream data.
func connectDown(ctx context.Context, cfg Config, host, path, auth, target, sessionID string) (net.Conn, *http.Response, error) {
	conn, err := dialTLS(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	var sb strings.Builder
	sb.WriteString("GET ")
	sb.WriteString(path)
	sb.WriteString(" HTTP/1.1\r\nHost: ")
	sb.WriteString(host)
	sb.WriteString("\r\nX-Naive-Target: ")
	sb.WriteString(target)
	sb.WriteString("\r\nX-Naive-Session: ")
	sb.WriteString(sessionID)
	sb.WriteString("\r\nContent-Type: application/octet-stream\r\n")
	if auth != "" {
		sb.WriteString("Proxy-Authorization: ")
		sb.WriteString(auth)
		sb.WriteString("\r\nX-Naive-Auth: ")
		sb.WriteString(auth)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")

	if _, err := io.WriteString(conn, sb.String()); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("write GET headers: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read GET response: %w", err)
	}
	return conn, resp, nil
}

// sendUpstream reads data from appConn and POSTs each chunk to the server.
// Runs until context is cancelled or an error occurs.
func sendUpstream(ctx context.Context, cfg Config, host, path, auth, sessionID string, appConn net.Conn) {
	buf := make([]byte, 16*1024)
	var upConn net.Conn

	defer func() {
		if upConn != nil {
			upConn.Close()
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		n, err := appConn.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			if perr := postChunk(ctx, cfg, host, path, auth, sessionID, chunk, &upConn); perr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// postChunk sends one upstream chunk as a POST request with Content-Length.
// Reuses upConn across calls (keep-alive); reopens on error.
func postChunk(ctx context.Context, cfg Config, host, path, auth, sessionID string, data []byte, upConn *net.Conn) error {
	// Try with existing connection first; on any error open a new one.
	for attempt := 0; attempt < 2; attempt++ {
		if *upConn == nil {
			c, err := dialTLS(ctx, cfg)
			if err != nil {
				return err
			}
			*upConn = c
		}

		if err := doPost(host, path, auth, sessionID, data, *upConn); err != nil {
			(*upConn).Close()
			*upConn = nil
			if attempt == 1 {
				return err
			}
			continue
		}
		return nil
	}
	return nil
}

func doPost(host, path, auth, sessionID string, data []byte, conn net.Conn) error {
	var sb strings.Builder
	sb.WriteString("POST ")
	sb.WriteString(path)
	sb.WriteString(" HTTP/1.1\r\nHost: ")
	sb.WriteString(host)
	sb.WriteString("\r\nX-Naive-Session: ")
	sb.WriteString(sessionID)
	sb.WriteString(fmt.Sprintf("\r\nContent-Length: %d\r\nContent-Type: application/octet-stream\r\nConnection: keep-alive\r\n", len(data)))
	if auth != "" {
		sb.WriteString("Proxy-Authorization: ")
		sb.WriteString(auth)
		sb.WriteString("\r\nX-Naive-Auth: ")
		sb.WriteString(auth)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")

	if _, err := io.WriteString(conn, sb.String()); err != nil {
		return err
	}
	if _, err := conn.Write(data); err != nil {
		return err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream POST returned %d", resp.StatusCode)
	}
	return nil
}
