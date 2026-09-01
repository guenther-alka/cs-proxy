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

var version = "0.12.0"

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

	// Optional AI edge (local LLM reverse proxy, e.g. llama-server / Ollama).
	// Listens on its own ports (ai_listen_http/https). Backends are selected by
	// PATH PREFIX: ai_upstream = default (no prefix), ai_upstream_<name> is
	// routed under /<name>/. Auth: ai_keys_file (Bearer, one key per line,
	// hot-reloaded on change) + ai_allowed_ip (IP/CIDR). The edge key is
	// stripped before the upstream hop; ai_upstream_key re-injects one.
	AIEnabled     bool
	AIListenAddr  string
	AIListenHTTP  string
	AIListenHTTPS string
	AIUpstream    string
	AIUpstreams   map[string]string // ai_upstream_<name> -> URL (path prefix /<name>/)
	AIUpstreamKey string            // optional Bearer injected to backends (edge key is stripped)
	AIKeysFile    string            // one key per line; hot-reloaded on change
	AIKeysExtra   []string          // deprecated ai_key values (still accepted, warn)
	AIAllowedIP   string
	AICertFile    string
	AIKeyFile     string

	ProxyAllowedIP string // GUI edge IP/CIDR allowlist (remote clients only; loopback always OK)
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

		AIEnabled:     false,
		AIListenAddr:  "0.0.0.0",
		AIListenHTTP:  "0", // "0" = HTTP listener off
		AIListenHTTPS: "8443",
		// "auto" = prefer a local llama-server, fall back to Ollama, live
		// re-checked (see ai.go). So ai=on alone, with nothing else set,
		// just works. Set ai_upstream=off to disable the default backend
		// (named ai_upstream_<name> backends still work), or to a literal
		// URL to forward there explicitly instead of auto-selecting.
		AIUpstream:    "auto",
		AIUpstreams:   map[string]string{},
		AIUpstreamKey: "",
		AIKeysFile:    "",
		AIAllowedIP:   "",
		AICertFile:    "",
		AIKeyFile:     "",

		ProxyAllowedIP: "",
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
		// optional AI edge (local LLM reverse proxy)
		case "ai":
			cfg.AIEnabled = on(v)
		case "ai_listen_addr":
			cfg.AIListenAddr = v
		case "ai_listen_http":
			cfg.AIListenHTTP = v
		case "ai_listen_https":
			cfg.AIListenHTTPS = v
		case "ai_upstream":
			cfg.AIUpstream = v
		case "ai_keys_file":
			cfg.AIKeysFile = v
		case "ai_upstream_key":
			cfg.AIUpstreamKey = v
		case "ai_key":
			log.Printf("cs-proxy: WARNING ai_key is deprecated -- put the key(s) into ai_keys_file (default _cfg/cs-proxy.keys) instead\n")
			cfg.AIKeysExtra = append(cfg.AIKeysExtra, v)
		case "ai_allowed_ip":
			cfg.AIAllowedIP = v
		case "ai_cert":
			cfg.AICertFile = v
		case "ai_key_file":
			cfg.AIKeyFile = v
		case "proxy_allowed_ip":
			cfg.ProxyAllowedIP = v
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
		// named AI backends: ai_upstream_<name> -> routed under /<name>/
		default:
			if strings.HasPrefix(k, "ai_upstream_") && k != "ai_upstream_key" {
				if name := strings.TrimPrefix(k, "ai_upstream_"); name != "" {
					cfg.AIUpstreams[name] = v
				}
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
	for _, p := range []*string{&cfg.CertFile, &cfg.KeyFile, &cfg.AICertFile, &cfg.AIKeyFile, &cfg.AIKeysFile} {
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
	var aiForward string
	var aiHTTP, aiHTTPS, aiAddr, aiAllowedIP, aiKeysFile string
	var allowedProxy string
	flag.StringVar(&cfgPath, "config", "", "path to config file (default: _cfg/webserver/webserver.conf next to the binary)")
	flag.StringVar(&cfgPath, "conf", "", "alias for -config")
	flag.StringVar(&forward, "forward", "", "upstream URL to reverse-proxy to (standalone/universal mode; overrides config)")
	flag.StringVar(&listenHTTP, "http", "", "HTTP listen port (overrides config; standalone convenience)")
	flag.StringVar(&listenHTTPS, "https", "", "HTTPS listen port (overrides config; standalone convenience)")
	flag.StringVar(&listenAddr, "addr", "", "listen address (overrides config; standalone convenience)")
	flag.StringVar(&aiForward, "ai-forward", "", "AI edge default upstream URL (local LLM reverse proxy; enables the AI listener)")
	flag.StringVar(&aiHTTP, "ai-http", "", "AI HTTP listen port (0 = off; overrides config)")
	flag.StringVar(&aiHTTPS, "ai-https", "", "AI HTTPS listen port (0 = off; overrides config)")
	flag.StringVar(&aiAddr, "ai-addr", "", "AI listen address (overrides config)")
	flag.StringVar(&aiKeysFile, "ai-keys-file", "", "AI edge Bearer key file (one key per line; hot-reloaded on change)")
	flag.StringVar(&aiAllowedIP, "allowed-ai", "", "AI edge client IP/CIDR allowlist (alias: -ai-allowed-ip)")
	flag.StringVar(&aiAllowedIP, "ai-allowed-ip", "", "alias for -allowed-ai")
	flag.StringVar(&allowedProxy, "allowed-proxy", "", "web GUI client IP/CIDR allowlist (remote only; loopback always allowed)")
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
	// AI edge overrides (standalone convenience for a dedicated AI server).
	// ai-forward alone = AI-only (no GUI edge); both --forward and --ai-forward
	// = both edges; the SOHO "both in one instance" mode normally comes from
	// the config file (proxy=on + ai=on).
	if aiForward != "" {
		cfg.AIEnabled = true
		cfg.AIUpstream = aiForward
		if forward == "" {
			cfg.Enabled = false
		}
	}
	if aiHTTP != "" {
		cfg.AIListenHTTP = aiHTTP
	}
	if aiHTTPS != "" {
		cfg.AIListenHTTPS = aiHTTPS
	}
	if aiAddr != "" {
		cfg.AIListenAddr = aiAddr
	}
	if aiKeysFile != "" {
		cfg.AIKeysFile = aiKeysFile
	}
	if aiAllowedIP != "" {
		cfg.AIAllowedIP = aiAllowedIP
	}
	if allowedProxy != "" {
		cfg.ProxyAllowedIP = allowedProxy
	}

	// aiDefaultActive mirrors ai.go's newAIEdge(): "" or "off" = no default
	// backend; "auto" or a literal URL both count as active.
	aiDefaultActive := func(s string) bool {
		s = strings.TrimSpace(s)
		return s != "" && !strings.EqualFold(s, "off")
	}
	aiOn := cfg.AIEnabled && (aiDefaultActive(cfg.AIUpstream) || len(cfg.AIUpstreams) > 0)
	if !cfg.Enabled && !aiOn {
		log.Printf("cs-proxy: proxy=off and ai=off in %s -- not starting (webserver.pl serves directly)\n", cfgPath)
		return
	}

	app := newApp(cfg)
	if cfg.Enabled {
		log.Printf("cs-proxy %s: web edge docroot=%s upstream=%s\n", version, cfg.Docroot, cfg.Upstream)
	} else {
		log.Printf("cs-proxy %s: web edge off (ai-only mode)\n", version)
	}

	// ---- GUI/web edge listeners (only when proxy=on) ----
	if cfg.Enabled {
		// HTTP listener ("0" or empty = off)
		if cfg.ListenHTTP != "" && cfg.ListenHTTP != "0" {
			go func() {
				addr := cfg.ListenAddr + ":" + cfg.ListenHTTP
				log.Printf("cs-proxy %s: HTTP listening on %s\n", version, addr)
				srv := &http.Server{Addr: addr, Handler: app, ReadHeaderTimeout: 10 * time.Second}
				if err := srv.ListenAndServe(); err != nil {
					log.Printf("cs-proxy: HTTP on %s failed: %v\n", addr, err)
					os.Exit(1)
				}
			}()
		}
		// HTTPS listener ("0" or empty = off; self-signed cert if none configured)
		if cfg.ListenHTTPS != "" && cfg.ListenHTTPS != "0" {
			go func() {
				tlsCfg, err := tlsConfigFor(app.certDir, cfg.CertFile, cfg.KeyFile)
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
			}()
		}
	}

	// ---- AI edge listeners (optional local LLM reverse proxy) ----
	if aiOn {
		edge := newAIEdge(cfg)
		keyLog := "off"
		if cfg.AIKeysFile != "" || len(cfg.AIKeysExtra) > 0 {
			keyLog = "on"
		}
		backends := cfg.AIUpstream
		for name := range cfg.AIUpstreams {
			if backends != "" {
				backends += ", "
			}
			backends += name + "=" + cfg.AIUpstreams[name]
		}
		log.Printf("cs-proxy %s: ai edge backends=%s key=%s keys_file=%s allowed_ip=%s\n",
			version, backends, keyLog, cfg.AIKeysFile, cfg.AIAllowedIP)
		if cfg.AIListenHTTP != "" && cfg.AIListenHTTP != "0" {
			go func() {
				addr := cfg.AIListenAddr + ":" + cfg.AIListenHTTP
				log.Printf("cs-proxy %s: AI HTTP listening on %s\n", version, addr)
				srv := &http.Server{Addr: addr, Handler: edge, ReadHeaderTimeout: 10 * time.Second}
				if err := srv.ListenAndServe(); err != nil {
					log.Printf("cs-proxy: AI HTTP on %s failed: %v\n", addr, err)
					os.Exit(1)
				}
			}()
		}
		if cfg.AIListenHTTPS != "" && cfg.AIListenHTTPS != "0" {
			go func() {
				aiTLS, err := tlsConfigFor(app.certDir, cfg.AICertFile, cfg.AIKeyFile)
				if err != nil {
					log.Printf("cs-proxy: AI TLS setup failed: %v\n", err)
					os.Exit(1)
				}
				addr := cfg.AIListenAddr + ":" + cfg.AIListenHTTPS
				log.Printf("cs-proxy %s: AI HTTPS listening on %s\n", version, addr)
				srv := &http.Server{Addr: addr, Handler: edge, TLSConfig: aiTLS, ReadHeaderTimeout: 10 * time.Second}
				if err := srv.ListenAndServeTLS("", ""); err != nil {
					log.Printf("cs-proxy: AI HTTPS on %s failed: %v\n", addr, err)
					os.Exit(1)
				}
			}()
		}
	}

	// keep the process alive (listeners are goroutines)
	select {}
}
