// Package tunnel implements the CDN-compatible POST tunnel transport for naivecdn.
// It establishes a bidirectional stream over POST /tunnel, using utls to present
// a Chrome TLS fingerprint to CDNs that perform TLS inspection.
package tunnel

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"

	"github.com/hardw3q/naivecdn/padding"
)

// Config holds the proxy connection parameters.
type Config struct {
	// ProxyURL is the full HTTPS URL of the proxy, including credentials.
	// Example: "https://user:pass@sativa.pxls-cdn.ru"
	ProxyURL string

	// TunnelPath is the POST endpoint path on the proxy. Defaults to "/tunnel".
	TunnelPath string

	// TLSFingerprint selects the utls ClientHello preset.
	// Supported: "chrome_auto" (default), "chrome_120", "firefox_120".
	TLSFingerprint string
}

// chunkedWriter wraps a net.Conn and writes data as HTTP/1.1 chunked encoding.
type chunkedWriter struct {
	conn net.Conn
}

func (cw *chunkedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// chunk-size\r\n
	header := []byte(fmt.Sprintf("%x\r\n", len(p)))
	if _, err := cw.conn.Write(header); err != nil {
		return 0, err
	}
	n, err := cw.conn.Write(p)
	if err != nil {
		return n, err
	}
	// CRLF after chunk data
	if _, err := cw.conn.Write([]byte("\r\n")); err != nil {
		return n, err
	}
	return n, nil
}

func (cw *chunkedWriter) CloseWrite() error {
	// HTTP/1.1 final chunk signals end of request body to the server.
	_, err := cw.conn.Write([]byte("0\r\n\r\n"))
	return err
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

func buildPaddingHeader() string {
	// [16, 32] symbols — same spec as client CONNECT request padding
	paddingLen := rand.Intn(17) + 16
	buf := make([]byte, paddingLen)
	bits := rand.Uint64()
	for i := 0; i < 8 && i < paddingLen; i++ {
		buf[i] = "!#$()+<>?@[]^`{}"[bits&15]
		bits >>= 4
	}
	for i := 8; i < paddingLen; i++ {
		buf[i] = '~'
	}
	return string(buf)
}

// Stream establishes a POST tunnel to the proxy and copies data bidirectionally
// between appConn and the remote target. Blocks until the tunnel closes.
func Stream(ctx context.Context, cfg Config, target string, appConn net.Conn) error {
	u, err := url.Parse(cfg.ProxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy URL: %w", err)
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	hostport := net.JoinHostPort(host, port)

	// Dial and TLS handshake with Chrome fingerprint.
	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", hostport)
	if err != nil {
		return fmt.Errorf("dial %s: %w", hostport, err)
	}

	fingerprint := chooseFingerprint(cfg.TLSFingerprint)
	uconn := utls.UClient(rawConn, &utls.Config{ServerName: host}, fingerprint)
	if err := uconn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return fmt.Errorf("TLS handshake: %w", err)
	}

	proto := uconn.ConnectionState().NegotiatedProtocol

	tunnelPath := cfg.TunnelPath
	if tunnelPath == "" {
		tunnelPath = "/tunnel"
	}

	creds := ""
	if u.User != nil {
		user := u.User.Username()
		pass, _ := u.User.Password()
		creds = base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	}

	paddingHdr := buildPaddingHeader()

	var (
		resp       *http.Response
		bodyWriter io.Writer // where we write upstream data (app→server)
	)

	switch proto {
	case "h2":
		resp, bodyWriter, err = streamH2(ctx, uconn, u, tunnelPath, target, creds, paddingHdr)
	default:
		resp, bodyWriter, err = streamH1(uconn, u, tunnelPath, target, creds, paddingHdr)
	}
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("proxy returned %d", resp.StatusCode)
	}

	hasPadding := resp.Header.Get("Padding") != ""
	defer resp.Body.Close()

	// Bidirectional stream with NaiveProxy padding (same as server's dualStream).
	//   appConn → bodyWriter (AddPadding) → CDN → server → target
	//   target → server (AddPadding) → CDN → resp.Body (RemovePadding) → appConn
	return padding.DualStream(appConn, resp.Body, bodyWriter, hasPadding)
}

// streamH2 sends the POST tunnel request over HTTP/2 using a utls connection.
// Returns before the request body is fully sent — HTTP/2 is genuinely bidirectional.
func streamH2(ctx context.Context, uconn net.Conn, u *url.URL, tunnelPath, target, creds, paddingHdr string) (*http.Response, io.Writer, error) {
	tr := &http2.Transport{}
	cc, err := tr.NewClientConn(uconn)
	if err != nil {
		uconn.Close()
		return nil, nil, fmt.Errorf("h2 client conn: %w", err)
	}

	pr, pw := io.Pipe()

	reqURL := &url.URL{Scheme: "https", Host: u.Host, Path: tunnelPath}
	req, _ := http.NewRequestWithContext(ctx, "POST", reqURL.String(), pr)
	req.Header.Set("X-Naive-Target", target)
	req.Header.Set("Padding", paddingHdr)
	req.Header.Set("Content-Type", "application/octet-stream")
	if creds != "" {
		req.Header.Set("Proxy-Authorization", "Basic "+creds)
	}

	// RoundTrip returns as soon as the server sends 200 OK HEADERS frame,
	// even while the request DATA frames are still being streamed.
	resp, err := cc.RoundTrip(req)
	if err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, fmt.Errorf("h2 round trip: %w", err)
	}

	// pw is the upstream write channel; closing it sends END_STREAM to server.
	return resp, pw, nil
}

// streamH1 sends the POST tunnel request over HTTP/1.1 with chunked encoding.
// Writes request headers first, then returns so the caller can stream the body
// while simultaneously reading the response — this is valid because TCP is full-duplex.
func streamH1(uconn net.Conn, u *url.URL, tunnelPath, target, creds, paddingHdr string) (*http.Response, io.Writer, error) {
	var sb strings.Builder
	sb.WriteString("POST ")
	sb.WriteString(tunnelPath)
	sb.WriteString(" HTTP/1.1\r\n")
	sb.WriteString("Host: ")
	sb.WriteString(u.Host)
	sb.WriteString("\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n")
	sb.WriteString("X-Naive-Target: ")
	sb.WriteString(target)
	sb.WriteString("\r\nPadding: ")
	sb.WriteString(paddingHdr)
	sb.WriteString("\r\n")
	if creds != "" {
		sb.WriteString("Proxy-Authorization: Basic ")
		sb.WriteString(creds)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")

	if _, err := io.WriteString(uconn, sb.String()); err != nil {
		uconn.Close()
		return nil, nil, fmt.Errorf("write request headers: %w", err)
	}

	// Read response headers. The server sends 200 OK + Padding header
	// immediately after parsing the request headers (Fast Open pattern).
	// TCP is full-duplex: we can stream the request body concurrently.
	br := bufio.NewReader(uconn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		uconn.Close()
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	cw := &chunkedWriter{conn: uconn}
	return resp, cw, nil
}
