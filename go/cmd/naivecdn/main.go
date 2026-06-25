// naivecdn — CDN-compatible NaiveProxy client.
//
// Uses Chrome TLS fingerprint (utls) and POST /tunnel instead of HTTP CONNECT,
// enabling NaiveProxy to work behind CDNs that block the CONNECT method.
//
// Config (same format as naive's config.json, with extra tunnel_path field):
//
//	{
//	  "listen":      "socks://127.0.0.1:1080",
//	  "proxy":       "https://user:pass@sativa.pxls-cdn.ru",
//	  "tunnel_path": "/tunnel",
//	  "fingerprint": "chrome_auto"
//	}
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hardw3q/naivecdn/socks5"
	"github.com/hardw3q/naivecdn/tunnel"
)

type Config struct {
	Listen             string `json:"listen"`
	Proxy              string `json:"proxy"`
	TunnelPath         string `json:"tunnel_path"`
	Fingerprint        string `json:"fingerprint"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	DNSServer          string `json:"dns_server"`
}

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = "socks://127.0.0.1:1080"
	}
	if cfg.TunnelPath == "" {
		cfg.TunnelPath = "/tunnel"
	}
	if cfg.Fingerprint == "" {
		cfg.Fingerprint = "chrome_auto"
	}

	listenAddr, err := parseSocksListen(cfg.Listen)
	if err != nil {
		log.Fatalf("bad listen address: %v", err)
	}

	srv := &socks5.Server{
		ListenAddr: listenAddr,
		TunnelCfg: tunnel.Config{
			ProxyURL:           cfg.Proxy,
			TunnelPath:         cfg.TunnelPath,
			TLSFingerprint:     cfg.Fingerprint,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
			DNSServer:          cfg.DNSServer,
		},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("naivecdn listening on %s → %s%s\n", listenAddr, cfg.Proxy, cfg.TunnelPath)

	if err := srv.ListenAndServe(ctx); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// parseSocksListen extracts "host:port" from a "socks://host:port" URL.
func parseSocksListen(listen string) (string, error) {
	listen = strings.TrimPrefix(listen, "socks://")
	listen = strings.TrimPrefix(listen, "socks5://")
	if listen == "" {
		return "", fmt.Errorf("empty listen address")
	}
	return listen, nil
}
