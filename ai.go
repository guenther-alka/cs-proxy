package main

import (
"crypto/subtle"
"encoding/json"
"fmt"
"log"
"net"
"net/http"
"net/http/httputil"
"net/url"
"os"
"sort"
"strings"
"sync"
"time"
)

// aiBackend is one upstream of the AI edge. The default backend (ai_upstream)
// has no prefix; named backends (ai_upstream_<name>) are routed under /<name>/.
type aiBackend struct {
name     string
prefix   string // "/<name>" or ""
upstream string
proxy    *httputil.ReverseProxy
}

func newBackend(name, prefix, upstream, injectKey string) *aiBackend {
up, err := url.Parse(upstream)
if err != nil {
up, _ = url.Parse("http://127.0.0.1:8080")
}
b := &aiBackend{name: name, prefix: prefix, upstream: upstream}
rp := &httputil.ReverseProxy{
Rewrite: func(r *httputil.ProxyRequest) {
r.SetURL(up)
if prefix != "" && strings.HasPrefix(r.Out.URL.Path, prefix) {
r.Out.URL.Path = strings.TrimPrefix(r.Out.URL.Path, prefix)
if r.Out.URL.Path == "" {
r.Out.URL.Path = "/"
}
}
r.Out.Host = up.Host
// Auth strip: the edge key is checked at the proxy and the
// backend never needs it (it must not end up in LLM logs).
r.Out.Header.Del("Authorization")
if injectKey != "" {
r.Out.Header.Set("Authorization", "Bearer "+injectKey)
}
},
}
rp.FlushInterval = -1 // SSE/token streaming passes through unbuffered
rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
w.Header().Set("X-CsProxy", "error")
msg := fmt.Sprintf("cs-proxy: could not connect the AI backend %s on %s -- is llama-server/Ollama running?\n",
b.label(), up.Host)
http.Error(w, msg, http.StatusBadGateway)
}
b.proxy = rp
return b
}

func (b *aiBackend) label() string {
if b.name == "" {
return "(default)"
}
return b.name
}

// aiEdge is the optional local-LLM reverse proxy (llama-server / Ollama).
// It listens on its own ports (ai_listen_http/https). Clients pick a backend
// by path prefix (/<name>/...); the edge key (ai_keys_file, hot-reloaded) and
// IP/CIDR allowlist (ai_allowed_ip) are enforced here only.
type aiEdge struct {
backends    []*aiBackend // named first (longest prefix), then default
defaultB    *aiBackend
mu          sync.Mutex
keysFile    string
keysMod     time.Time
keysLoaded  bool
keys        []string // merged: keys file (reloaded) + deprecated ai_key
allowed     []*net.IPNet
allowedIP   []net.IP
modelsCache []byte
modelsAt    time.Time
}

func newAIEdge(cfg Config) *aiEdge {
e := &aiEdge{keysFile: cfg.AIKeysFile}
e.keys = append(e.keys, cfg.AIKeysExtra...)
// named backends, longest prefix first (deterministic routing)
names := make([]string, 0, len(cfg.AIUpstreams))
for n := range cfg.AIUpstreams {
names = append(names, n)
}
sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
for _, n := range names {
e.backends = append(e.backends, newBackend(n, "/"+n, cfg.AIUpstreams[n], cfg.AIUpstreamKey))
}
if cfg.AIUpstream != "" {
e.defaultB = newBackend("", "", cfg.AIUpstream, cfg.AIUpstreamKey)
}
e.setAllowed(cfg.AIAllowedIP)
return e
}

func (e *aiEdge) setAllowed(s string) {
e.allowed, e.allowedIP = parseAllowedIPs(s)
}

// parseAllowedIPs splits a comma-separated IP/CIDR list into exact IPs + nets.
func parseAllowedIPs(s string) ([]*net.IPNet, []net.IP) {
var nets []*net.IPNet
var ips []net.IP
for _, part := range strings.Split(s, ",") {
part = strings.TrimSpace(part)
if part == "" {
continue
}
if strings.Contains(part, "/") {
if _, n, err := net.ParseCIDR(part); err == nil {
nets = append(nets, n)
}
} else if ip := net.ParseIP(part); ip != nil {
ips = append(ips, ip)
}
}
return nets, ips
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

// reloadKeys re-reads the key file when its mtime changes, so add/del in the
// settings GUI takes effect immediately (no cs-proxy restart). Keys are
// stored as the part before '=' (optional label after '=' is display-only).
func (e *aiEdge) reloadKeys() {
e.mu.Lock()
defer e.mu.Unlock()
if e.keysFile == "" {
return
}
fi, err := os.Stat(e.keysFile)
if err != nil {
return // file missing yet: keep current keys, retry next request
}
if e.keysLoaded && fi.ModTime().Equal(e.keysMod) {
return
}
data, err := os.ReadFile(e.keysFile)
if err != nil {
return
}
mod := fi.ModTime()
var ks []string
for _, line := range strings.Split(string(data), "\n") {
line = strings.TrimSpace(line)
if line == "" || strings.HasPrefix(line, "#") {
continue
}
if i := strings.Index(line, "="); i > 0 {
line = strings.TrimSpace(line[:i])
}
if line != "" {
ks = append(ks, line)
}
}
e.keys = e.keys[:0]
e.keys = append(e.keys, ks...)
e.keysMod = mod
e.keysLoaded = true
}

func (e *aiEdge) authOK(tok string) bool {
e.reloadKeys()
e.mu.Lock()
defer e.mu.Unlock()
if len(e.keys) == 0 {
return true // no keys configured: listener is open (TLS/IP still apply)
}
for _, k := range e.keys {
if subtle.ConstantTimeCompare([]byte(tok), []byte(k)) == 1 {
return true
}
}
return false
}

func (e *aiEdge) routeBackend(path string) *aiBackend {
for _, b := range e.backends {
if strings.HasPrefix(path, b.prefix) {
return b
}
}
return e.defaultB
}
// ServeHTTP authenticates and forwards one request to the matching AI backend.
// Every request is logged like the main edge (note column "ai"), including
// auth rejections.
func (e *aiEdge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
start := time.Now()
sw := &statusWriter{ResponseWriter: w}
// Bearer auth (constant-time compare against hot-reloaded keys)
tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
if !e.authOK(tok) {
sw.Header().Set("WWW-Authenticate", "Bearer")
http.Error(sw, "Unauthorized\n", http.StatusUnauthorized)
e.logStatus(sw, r, start)
return
}
// IP allowlist
if len(e.allowed) > 0 || len(e.allowedIP) > 0 {
if !ipAllowed(clientIP(r), e.allowed, e.allowedIP) {
http.Error(sw, "Forbidden\n", http.StatusForbidden)
e.logStatus(sw, r, start)
return
}
}
// aggregated /v1/models across all backends (only when >1 backend)
if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
r.URL.Path == "/v1/models" && len(e.backends) > 0 && e.defaultB != nil {
if e.serveModels(sw, r) {
e.logStatus(sw, r, start)
return
}
// aggregation failed -> fall through to the default backend
}
b := e.routeBackend(r.URL.Path)
if b == nil {
msg := "cs-proxy: no AI backend for " + r.URL.Path +
" -- configure ai_upstream (default) or ai_upstream_<name> and use /<name>/...\n"
http.Error(sw, msg, http.StatusNotFound)
e.logStatus(sw, r, start)
return
}
// forward (streaming: SSE/token chunks pass through)
b.proxy.ServeHTTP(sw, r)
e.logStatus(sw, r, start)
}

func (e *aiEdge) logStatus(sw *statusWriter, r *http.Request, start time.Time) {
status := sw.status
if status == 0 {
status = 200
}
log.Printf("HTTP %d %s %s %s %dms ai\n",
status, r.Method, r.URL.Path, clientIP(r), time.Since(start).Milliseconds())
}

// serveModels merges GET /v1/models from every backend into one list. Model
// ids are prefixed with the backend name (llama/qwen2.5:0.5b) so the GUI
// dropdown can tell them apart; the matching chat endpoint is /<name>/v1/...
// The merged list is cached for 10s to avoid hammering the backends.
func (e *aiEdge) serveModels(w http.ResponseWriter, r *http.Request) bool {
e.mu.Lock()
cache := e.modelsCache
age := time.Since(e.modelsAt)
e.mu.Unlock()
if cache != nil && age < 10*time.Second {
w.Header().Set("Content-Type", "application/json")
w.Write(cache)
return true
}
type model struct {
ID       string `json:"id"`
Object   string `json:"object"`
OwnedBy  string `json:"owned_by"`
Backend  string `json:"x_csproxy_backend,omitempty"`
Endpoint string `json:"x_csproxy_endpoint,omitempty"`
}
merged := map[string]bool{}
var out []model
backends := append([]*aiBackend{}, e.backends...)
if e.defaultB != nil {
backends = append(backends, e.defaultB)
}
for _, b := range backends {
for _, id := range e.fetchModels(b) {
full := id
ep := "/v1"
if b.name != "" {
full = b.name + "/" + id
ep = "/" + b.name + "/v1"
}
if merged[full] {
continue
}
merged[full] = true
out = append(out, model{ID: full, Object: "model", OwnedBy: "cs-proxy", Backend: b.name, Endpoint: ep})
}
}
body, err := json.Marshal(map[string]any{"object": "list", "data": out})
if err != nil {
return false
}
e.mu.Lock()
e.modelsCache = body
e.modelsAt = time.Now()
e.mu.Unlock()
w.Header().Set("Content-Type", "application/json")
w.Write(body)
return true
}

func (e *aiEdge) fetchModels(b *aiBackend) []string {
client := &http.Client{Timeout: 5 * time.Second}
resp, err := client.Get(b.upstream + "/v1/models")
if err != nil {
return nil
}
defer resp.Body.Close()
if resp.StatusCode != http.StatusOK {
return nil
}
var payload struct {
Data []struct {
ID string `json:"id"`
} `json:"data"`
}
if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
return nil
}
var ids []string
for _, m := range payload.Data {
if m.ID != "" {
ids = append(ids, m.ID)
}
}
return ids
}
