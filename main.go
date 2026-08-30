// cs-proxy -- HTTP/HTTPS edge for napp-it cs.
//
// Serves the static document root (data/wwwroot) directly (gzip, in-memory
// cache, ETag/304), terminates TLS, and reverse-proxies dynamic requests
// (/cgi-bin/* and anything not found in the docroot) to the Perl webserver
// (webserver.pl, plain HTTP).
//
// Ships inside the napp-it distribution at data/cs_server/proxy/<platform>.<arch>/,
// selected at runtime by the platform -- always available, no download needed.
//
// Config: the proxy settings live in the SAME file webserver.pl uses
// (_cfg/webserver/webserver.conf), under proxy_* keys; the upstream port is
// derived from http_port so the proxy always follows webserver.pl's actual
// port. No separate proxy config file.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var version = "0.1.0"

type Config struct {
	Enabled     bool
	ListenAddr  string
	ListenHTTP  string
	ListenHTTPS string
	Upstream    string
	Docroot     string
	CertFile    string
	KeyFile     string
	DefaultURL  string

	CacheOn   bool
	CacheMaxMB int64
	CacheTTLS  int

	Compress string // "", "gzip", "gzip,brotli"
}

func defaultConfig() Config {
	return Config{
		Enabled:     true,
		ListenAddr:  "0.0.0.0",
		ListenHTTP:  "80",
		ListenHTTPS: "443",
		Upstream:    "http://127.0.0.1:800",
		CertFile:    "",
		KeyFile:     "",
		DefaultURL:  "",

		CacheOn:    true,
		CacheMaxMB: 64,
		CacheTTLS:  60,

		Compress: "gzip",
	}
}

// loadConfig parses the shared webserver.conf (_cfg/webserver/webserver.conf).
// The proxy settings live there under proxy_* keys; http_port determines the
// upstream port (so the proxy always follows webserver.pl's actual port).
// A standalone _cfg/cs-proxy.cfg (deprecated) still works via -config.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	// derive docroot from the config location:
	//   <root>/_cfg/webserver/webserver.conf -> <root>/data/wwwroot
	//   <root>/_cfg/cs-proxy.cfg            -> <root>/data/wwwroot
	// Deterministic on every platform (the executable path cannot be resolved
	// on some OSes, e.g. illumos).
	cfgDir := "."
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		cfgDir = dir
	}
	isProxyOnly := filepath.Base(path) == "cs-proxy.cfg"
	if isProxyOnly {
		cfg.Docroot = filepath.Join(cfgDir, "..", "data", "wwwroot")
	} else {
		cfg.Docroot = filepath.Join(cfgDir, "..", "..", "data", "wwwroot")
	}
	// fallback for relative/in-memory configs: derive from the executable
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dir != "" {
			alt := filepath.Join(dir, "..", "..", "..", "wwwroot")
			if _, err := os.Stat(alt); err == nil {
				cfg.Docroot = alt
			}
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dir := filepath.Dir(path)

	upstreamSet := false
	httpPort := "800"

	on := func(v string) bool {
		return strings.EqualFold(v, "on") || strings.EqualFold(v, "yes") || v == "1"
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(strings.TrimSpace(v))
		switch k {
		// proxy section in webserver.conf
		case "proxy":
			cfg.Enabled = on(v)
		case "proxy_listen_addr":
			cfg.ListenAddr = v
		case "proxy_listen_http":
			cfg.ListenHTTP = v
		case "proxy_listen_https":
			cfg.ListenHTTPS = v
		case "proxy_upstream":
			cfg.Upstream = v
			upstreamSet = true
		case "proxy_cert":
			cfg.CertFile = v
		case "proxy_key":
			cfg.KeyFile = v
		case "proxy_cache":
			cfg.CacheOn = on(v)
		case "proxy_cache_max_mb":
			fmt.Sscanf(v, "%d", &cfg.CacheMaxMB)
		case "proxy_cache_ttl_s":
			fmt.Sscanf(v, "%d", &cfg.CacheTTLS)
		case "proxy_compress":
			cfg.Compress = strings.ToLower(v)
		// shared keys (also used by webserver.pl)
		case "http_port":
			httpPort = v
		case "default_url":
			cfg.DefaultURL = v
		// deprecated standalone cs-proxy.cfg keys (only accepted there, never
		// inside webserver.conf -- listen_addr/cert/... would collide with
		// webserver.pl's own keys)
		case "listen_addr", "upstream", "cert", "key", "cache", "cache_max_mb",
			"cache_ttl_s", "compress":
			if !isProxyOnly {
				continue
			}
			switch k {
			case "listen_addr":
				cfg.ListenAddr = v
			case "upstream":
				cfg.Upstream = v
				upstreamSet = true
			case "cert":
				cfg.CertFile = v
			case "key":
				cfg.KeyFile = v
			case "cache":
				cfg.CacheOn = on(v)
			case "cache_max_mb":
				fmt.Sscanf(v, "%d", &cfg.CacheMaxMB)
			case "cache_ttl_s":
				fmt.Sscanf(v, "%d", &cfg.CacheTTLS)
			case "compress":
				cfg.Compress = strings.ToLower(v)
			}
		}
	}

	// upstream: proxy_upstream/upstream override; default derives from
	// webserver.conf http_port so the proxy always follows webserver.pl.
	if !upstreamSet {
		if _, err := strconv.Atoi(httpPort); err == nil {
			cfg.Upstream = "http://127.0.0.1:" + httpPort
		}
	}
	if !filepath.IsAbs(cfg.Docroot) {
		cfg.Docroot = filepath.Join(dir, cfg.Docroot)
	}
	for _, p := range []*string{&cfg.CertFile, &cfg.KeyFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	return cfg, nil
}

func main() {
	var cfgPath string
	var forward string
	var listenHTTP, listenHTTPS, listenAddr string
	flag.StringVar(&cfgPath, "config", "", "path to config file (default: _cfg/webserver/webserver.conf next to the binary)")
	flag.StringVar(&cfgPath, "conf", "", "alias for -config")
	flag.StringVar(&forward, "forward", "", "upstream URL to reverse-proxy to (standalone/universal mode; overrides config)")
	flag.StringVar(&listenHTTP, "http", "", "HTTP listen port (overrides config; standalone convenience)")
	flag.StringVar(&listenHTTPS, "https", "", "HTTPS listen port (overrides config; standalone convenience)")
	flag.StringVar(&listenAddr, "addr", "", "listen address (overrides config; standalone convenience)")
	flag.Parse()

	if cfgPath == "" {
		if exe, err := os.Executable(); err == nil {
			// <...>/cs_server/proxy/<plat>.<arch>/cs-proxy
			//   -> <root>/_cfg/webserver/webserver.conf
			cfgPath = filepath.Join(filepath.Dir(exe), "..", "..", "..", "..", "_cfg", "webserver", "webserver.conf")
		}
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("cs-proxy: config warning: %v (using defaults + docroot)\n", err)
	}

	// standalone/universal overrides (no webserver.pl coupling)
	if forward != "" {
		cfg.Upstream = forward
		cfg.Enabled = true
	}
	if listenHTTP != "" {
		cfg.ListenHTTP = listenHTTP
	}
	if listenHTTPS != "" {
		cfg.ListenHTTPS = listenHTTPS
	}
	if listenAddr != "" {
		cfg.ListenAddr = listenAddr
	}

	if !cfg.Enabled {
		log.Printf("cs-proxy: proxy=off in %s -- not starting (webserver.pl serves directly)\n", cfgPath)
		return
	}

	app := newApp(cfg)
	log.Printf("cs-proxy %s: docroot=%s upstream=%s\n", version, cfg.Docroot, cfg.Upstream)

	// HTTP listener
	go func() {
		addr := cfg.ListenAddr + ":" + cfg.ListenHTTP
		log.Printf("cs-proxy %s: HTTP listening on %s\n", version, addr)
		srv := &http.Server{Addr: addr, Handler: app, ReadHeaderTimeout: 10 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("cs-proxy: HTTP on %s failed: %v\n", addr, err)
			os.Exit(1)
		}
	}()

	// HTTPS listener (self-signed cert if none configured)
	tlsCfg, err := app.tlsConfig()
	if err != nil {
		log.Printf("cs-proxy: TLS setup failed: %v\n", err)
		os.Exit(1)
	}
	addr := cfg.ListenAddr + ":" + cfg.ListenHTTPS
	log.Printf("cs-proxy %s: HTTPS listening on %s\n", version, addr)
	srv := &http.Server{Addr: addr, Handler: app, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServeTLS("", ""); err != nil {
		log.Printf("cs-proxy: HTTPS on %s failed: %v\n", addr, err)
		os.Exit(1)
	}
}
