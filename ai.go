package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// aiEdge is the optional local-LLM reverse proxy (llama-server / Ollama).
// It listens on its own ports (ai_listen_http/https) and forwards everything
// to ai_upstream, so clients reach the AI backend with TLS + optional
// Bearer-key/IP-allowlist auth through a single cs-proxy instance.
type aiEdge struct {
	proxy     *httputil.ReverseProxy
	key       string
	allowed   []*net.IPNet
	allowedIP []net.IP
	upstream  string
}

func newAIEdge(upstream string) *aiEdge {
	up, err := url.Parse(upstream)
	if err != nil {
		up, _ = url.Parse("http://127.0.0.1:8080")
	}
	rp := httputil.NewSingleHostReverseProxy(up)
	// FlushInterval < 0 disables buffering: SSE/token streaming (e.g. Ollama
	// /api/chat, llama-server /v1/chat/completions) is passed through.
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("X-CsProxy", "error")
		msg := fmt.Sprintf("cs-proxy: could not connect the AI backend on %s -- is llama-server/Ollama running?\n", up.Host)
		http.Error(w, msg, http.StatusBadGateway)
	}
	return &aiEdge{proxy: rp, upstream: upstream}
}

func (e *aiEdge) setKey(k string) { e.key = k }

// setAllowed parses a comma-separated list of IPs and CIDRs (e.g.
// "192.168.2.0/24,10.0.0.5") into the allowlist.
func (e *aiEdge) setAllowed(s string) {
	e.allowed = nil
	e.allowedIP = nil
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			if _, n, err := net.ParseCIDR(part); err == nil {
				e.allowed = append(e.allowed, n)
			}
		} else if ip := net.ParseIP(part); ip != nil {
			e.allowedIP = append(e.allowedIP, ip)
		}
	}
}

func ipAllowed(ip string, nets []*net.IPNet, ips []net.IP) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(p) {
			return true
		}
	}
	for _, a := range ips {
		if a.Equal(p) {
			return true
		}
	}
	return false
}

// ServeHTTP authenticates and forwards one request to the AI upstream.
// Every request is logged like the main edge (note column "ai"), including
// auth rejections.
func (e *aiEdge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w}
	// Bearer auth (constant-time compare)
	if e.key != "" {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(tok), []byte(e.key)) != 1 {
			sw.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(sw, "Unauthorized\n", http.StatusUnauthorized)
			status := sw.status
			if status == 0 {
				status = 200
			}
			log.Printf("HTTP %d %s %s %s %dms ai\n",
				status, r.Method, r.URL.Path, clientIP(r), time.Since(start).Milliseconds())
			return
		}
	}
	// IP allowlist
	if len(e.allowed) > 0 || len(e.allowedIP) > 0 {
		if !ipAllowed(clientIP(r), e.allowed, e.allowedIP) {
			http.Error(sw, "Forbidden\n", http.StatusForbidden)
			status := sw.status
			if status == 0 {
				status = 200
			}
			log.Printf("HTTP %d %s %s %s %dms ai\n",
				status, r.Method, r.URL.Path, clientIP(r), time.Since(start).Milliseconds())
			return
		}
	}
	// forward (streaming: SSE/token chunks pass through)
	e.proxy.ServeHTTP(sw, r)
	status := sw.status
	if status == 0 {
		status = 200
	}
	log.Printf("HTTP %d %s %s %s %dms ai\n",
		status, r.Method, r.URL.Path, clientIP(r), time.Since(start).Milliseconds())
}
