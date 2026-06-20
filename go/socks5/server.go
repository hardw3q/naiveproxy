// Package socks5 implements a minimal SOCKS5 server (no-auth, CONNECT only).
package socks5

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/hardw3q/naivecdn/tunnel"
)

// Server listens for SOCKS5 connections and forwards them through the CDN tunnel.
type Server struct {
	ListenAddr string
	TunnelCfg  tunnel.Config
}

// ListenAndServe starts the SOCKS5 listener and blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("socks5 listen %s: %w", s.ListenAddr, err)
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
	target, err := handshake(conn)
	if err != nil {
		return
	}
	_ = tunnel.Stream(ctx, s.TunnelCfg, target, conn)
}

// handshake performs the SOCKS5 handshake and returns the target "host:port".
func handshake(conn net.Conn) (string, error) {
	// Client greeting: [ver=5, nMethods, methods...]
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", err
	}
	if header[0] != 5 {
		return "", fmt.Errorf("unsupported SOCKS version %d", header[0])
	}
	nMethods := int(header[1])
	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", err
	}

	// Server choice: no authentication (0x00)
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return "", err
	}

	// Client request: [ver=5, cmd, rsv=0, atyp, addr, port_hi, port_lo]
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return "", err
	}
	if req[0] != 5 {
		return "", fmt.Errorf("unexpected SOCKS version in request: %d", req[0])
	}
	if req[1] != 1 {
		// Only CONNECT (0x01) supported
		sendReply(conn, 7) // command not supported
		return "", fmt.Errorf("unsupported SOCKS5 command: %d", req[1])
	}

	var host string
	switch req[3] {
	case 1: // IPv4
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	case 3: // Domain name
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", err
		}
		domain := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", err
		}
		host = string(domain)
	case 4: // IPv6
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	default:
		sendReply(conn, 8) // address type not supported
		return "", fmt.Errorf("unsupported SOCKS5 address type: %d", req[3])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portBuf)
	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	// Success reply: [ver=5, rep=0 (success), rsv=0, atyp=1 (IPv4), 0.0.0.0, port=0]
	sendReply(conn, 0)
	return target, nil
}

func sendReply(conn net.Conn, rep byte) {
	_, _ = conn.Write([]byte{5, rep, 0, 1, 0, 0, 0, 0, 0, 0})
}
