package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type App struct {
	cfg         Config
	proxy       *httputil.ReverseProxy
	cache       *StaticCache
	docroot     string
	docrootReal string // resolved symlinks (for containment checks)
	certDir     string
}

func newApp(cfg Config) *App {
	up, err := url.Parse(cfg.Upstream)
	if err != nil {
		up, _ = url.Parse("http://127.0.0.1:8000")
	}
	rp := httputil.NewSingleHostReverseProxy(up)
	// FlushInterval < 0 disables buffering: SSE/token streaming and
	// file transfers are passed through chunk-by-chunk, never buffered.
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("X-CsProxy", "error")
		host := cfg.Upstream
		if u, e := url.Parse(cfg.Upstream); e == nil && u.Host != "" {
			host = u.Host
		}
		// plaintext hint: the Perl backend is the usual suspect, so name it
		// (webserver.pl) plus the host:port it was expected on.
		msg := fmt.Sprintf("cs-proxy: could not connect webserver.pl on %s -- the webserver backend is not running.\n", host)
		msg += "Start it with start.pl (option 1, 4 or 9) and reload this page.\n"
		http.Error(w, msg, http.StatusBadGateway)
	}
	certDir := ""
	if exe, err := os.Executable(); err == nil {
		// <...>/cs_server/proxy/<plat>.<arch>/cs-proxy -> <...>/_cfg
		certDir = filepath.Join(filepath.Dir(exe), "..", "..", "..", "..", "_cfg")
	}
	docrootReal := cfg.Docroot
	if res, err := filepath.EvalSymlinks(cfg.Docroot); err == nil {
		docrootReal = res
	}
	return &App{
		cfg:         cfg,
		proxy:       rp,
		cache:       newStaticCache(cfg.CacheMaxMB*1024*1024, cfg.CacheTTLS),
		docroot:     cfg.Docroot,
		docrootReal: docrootReal,
		certDir:     certDir,
	}
}

// tlsConfigFor returns a TLS config for a listener, generating + persisting a
// self-signed cert when none is configured (stable across restarts). The GUI
// edge and the AI edge share the same generated cert (cs-proxy-cert.pem in
// _cfg/) unless a listener-specific cert/key is configured.
func tlsConfigFor(certDir, certFile, keyFile string) (*tls.Config, error) {
	if certFile != "" && keyFile != "" {
		if cert, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
			return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}, nil
		}
	}
	certPath := filepath.Join(certDir, "cs-proxy-cert.pem")
	keyPath := filepath.Join(certDir, "cs-proxy-key.pem")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}, nil
	}
	certPEM, keyPEM, err := generateSelfSigned("cs-proxy")
	if err != nil {
		return nil, err
	}
	if os.MkdirAll(certDir, 0700) == nil {
		os.WriteFile(certPath, certPEM, 0600)
		os.WriteFile(keyPath, keyPEM, 0600)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}, nil
}

// tlsConfig for the GUI edge (uses the configured proxy cert/key, else the
// shared generated cert). Kept for compatibility; listeners call tlsConfigFor.
func (a *App) tlsConfig() (*tls.Config, error) {
	return tlsConfigFor(a.certDir, a.cfg.CertFile, a.cfg.KeyFile)
}

func generateSelfSigned(host string) ([]byte, []byte, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().Unix()),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// statusWriter tracks the response status/bytes and a short note so the
// per-request console log line matches webserver.pl's "HTTP 200 GET /path
// client 12ms note" format. Must preserve the underlying writer's Flusher
// (ReverseProxy streaming/SSE) and Hijacker (upgrades) capabilities.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	note   string
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("cs-proxy: hijack not supported")
}

// ServeHTTP logs every request to stdout (console) like webserver.pl:
//   HTTP 200 GET /cgi-bin/admin.pl 127.0.0.1 12ms proxy
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	a.route(sw, r)
	status := sw.status
	if status == 0 {
		status = 200
	}
	log.Printf("HTTP %d %s %s %s %dms %s\n",
		status, r.Method, r.URL.Path, clientIP(r),
		time.Since(start).Milliseconds(), sw.note)
}

// ServeHTTP is the single entry point for HTTP and HTTPS.
func (a *App) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	isHTTPS := r.TLS != nil
	remote := clientIP(r)

	// monitor / watchdog endpoints
	if path == "/healthz" || path == "/ping" {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok")
		if sw, ok := w.(*statusWriter); ok {
			sw.note = "ping"
		}
		return
	}

	// graceful-shutdown endpoint: block at the edge -- only a true loopback
	// client may reach webserver.pl's /closeme_<token> directly (the proxy
	// must not expose the shared-secret shutdown to remote clients).
	if strings.HasPrefix(path, "/closeme_") {
		http.Error(w, "Forbidden\n", http.StatusForbidden)
		return
	}

	// HTTP (port 80) from a REMOTE client: forcibly forward to the edge's
	// https (B4) -- every path, instant 302, so AJAX/JSON follows too.
	// Loopback (localhost test access, the proxy's own health checks) is
	// served directly.
	if !isHTTPS && !isLoopback(remote) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		u := "https://" + host + r.URL.Path
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, u, http.StatusFound)
		if sw, ok := w.(*statusWriter); ok {
			sw.note = "http->https"
		}
		return
	}

	// root redirect (HTTPS or loopback only)
	if path == "/" || path == "" {
		if sw, ok := w.(*statusWriter); ok {
			sw.note = "root"
		}
		a.handleRoot(w, r, isHTTPS, remote)
		return
	}

	// dynamic -> reverse proxy (always, so SSE/upload streaming is kept)
	if strings.HasPrefix(path, "/cgi-bin/") {
		if sw, ok := w.(*statusWriter); ok {
			sw.note = "proxy"
		}
		a.proxy.ServeHTTP(w, r)
		return
	}

	// static: serve from the docroot if the file exists
	if a.serveStatic(w, r, path) {
		if sw, ok := w.(*statusWriter); ok {
			sw.note = "static"
		}
		return
	}

	// anything else (not found in docroot) -> upstream (it may 404 or generate)
	if sw, ok := w.(*statusWriter); ok {
		sw.note = "proxy"
	}
	a.proxy.ServeHTTP(w, r)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopback(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "127.")
}

// handleRoot: HTTPS/loopback -> instant redirect to default_url. Remote
// HTTP never reaches this handler (route() redirects it to https first).
func (a *App) handleRoot(w http.ResponseWriter, r *http.Request, isHTTPS bool, remote string) {
	def := a.defURLForHost(r.Host)
	w.Header().Set("Location", def)
	w.WriteHeader(http.StatusFound)
}

func (a *App) defURLForHost(host string) string {
	if a.cfg.DefaultURL != "" {
		return a.cfg.DefaultURL
	}
	return "http://" + host + "/"
}

