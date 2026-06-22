// Package socks5 implements a minimal SOCKS5 + HTTP-CONNECT proxy server (no-auth).
package socks5

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/hardw3q/naivecdn/tunnel"
)

// Server listens for SOCKS5 or HTTP CONNECT connections and forwards them through the CDN tunnel.
type Server struct {
	ListenAddr string
	TunnelCfg  tunnel.Config
}

// ListenAndServe starts the listener and blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.ListenAddr, err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Peek at first byte to auto-detect protocol:
	// 0x05 = SOCKS5, anything else = HTTP (CONNECT method starts with 'C' = 0x43)
	first := make([]byte, 1)
	if _, err := io.ReadFull(conn, first); err != nil {
		return
	}
	r := io.MultiReader(bytes.NewReader(first), conn)

	var target string
	var err error
	if first[0] == 5 {
		target, err = handshakeSocks5(r, conn)
	} else {
		target, err = handshakeHTTP(r, conn)
	}
	if err != nil {
		return
	}
	_ = tunnel.Stream(ctx, s.TunnelCfg, target, conn)
}

// handshakeSocks5 performs the SOCKS5 handshake and returns the target "host:port".
// r is used for reading (may have a prepended byte), w for writing replies.
func handshakeSocks5(r io.Reader, w io.Writer) (string, error) {
	// Client greeting: [ver=5, nMethods, methods...]
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return "", err
	}
	if header[0] != 5 {
		return "", fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	nMethods := int(header[1])
	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(r, methods); err != nil {
		return "", err
	}

	// Server choice: no authentication (0x00)
	if _, err := w.Write([]byte{5, 0}); err != nil {
		return "", err
	}

	// Client request: [ver=5, cmd, rsv=0, atyp, addr, port_hi, port_lo]
	req := make([]byte, 4)
	if _, err := io.ReadFull(r, req); err != nil {
		return "", err
	}
	if req[0] != 5 {
		return "", fmt.Errorf("unexpected SOCKS version in request: %d", req[0])
	}
	if req[1] != 1 {
		sendSocks5Reply(w, 7) // command not supported
		return "", fmt.Errorf("unsupported SOCKS5 command: %d", req[1])
	}

	var host string
	switch req[3] {
	case 1: // IPv4
		addr := make([]byte, 4)
		if _, err := io.ReadFull(r, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	case 3: // Domain name
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return "", err
		}
		domain := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(r, domain); err != nil {
			return "", err
		}
		host = string(domain)
	case 4: // IPv6
		addr := make([]byte, 16)
		if _, err := io.ReadFull(r, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	default:
		sendSocks5Reply(w, 8) // address type not supported
		return "", fmt.Errorf("unsupported SOCKS5 address type: %d", req[3])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, portBuf); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portBuf)
	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	sendSocks5Reply(w, 0) // success
	return target, nil
}

func sendSocks5Reply(w io.Writer, rep byte) {
	_, _ = w.Write([]byte{5, rep, 0, 1, 0, 0, 0, 0, 0, 0})
}

// handshakeHTTP handles an HTTP CONNECT request and returns the target "host:port".
func handshakeHTTP(r io.Reader, w io.Writer) (string, error) {
	br := bufio.NewReader(r)
	req, err := http.ReadRequest(br)
	if err != nil {
		return "", fmt.Errorf("read HTTP request: %w", err)
	}
	if req.Method != http.MethodConnect {
		_, _ = w.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return "", fmt.Errorf("unsupported HTTP method: %s", req.Method)
	}
	target := req.Host // "host:port" from CONNECT host:port HTTP/1.1
	if target == "" {
		_, _ = w.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return "", fmt.Errorf("missing CONNECT target")
	}
	_, _ = w.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	return target, nil
}
