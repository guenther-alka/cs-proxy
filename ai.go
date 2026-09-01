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
// proxy is an http.Handler so both a plain static *httputil.ReverseProxy and
// the dynamic *autoUpstream (ai_upstream=auto) can serve as a backend.
type aiBackend struct {
name     string
prefix   string // "/<name>" or ""
upstream string // literal config value ("auto" for the dynamic backend)
proxy    http.Handler
auto     *autoUpstream // non-nil only for the ai_upstream=auto backend
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

// resolvedUpstream returns the URL this backend is currently forwarding to.
// For a static backend that is simply b.upstream; for the dynamic auto
// backend it is whichever candidate is currently live (or "" if none is).
func (b *aiBackend) resolvedUpstream() string {
if b.auto != nil {
return b.auto.resolvedURL()
}
return b.upstream
}

// -- ai_upstream=auto --------------------------------------------------
//
// Dynamic default backend: prefers a local llama-server, falls back to
// Ollama, re-checked periodically (not just once at config-load time) so
// starting/stopping either backend on the host is picked up live.

const autoRecheckInterval = 5 * time.Second
const autoProbeTimeout = 800 * time.Millisecond

type autoCandidate struct {
label string
url   string
proxy *httputil.ReverseProxy
}

type autoUpstream struct {
candidates []*autoCandidate
mu         sync.Mutex
current    *autoCandidate
checkedAt  time.Time
}

// newAutoUpstream builds the fixed candidate list in priority order:
// llama-server (1st preference) then Ollama (2nd preference).
func newAutoUpstream(injectKey string) *autoUpstream {
mk := func(label, upstream string) *autoCandidate {
b := newBackend("", "", upstream, injectKey)
return &autoCandidate{label: label, url: upstream, proxy: b.proxy.(*httputil.ReverseProxy)}
}
return &autoUpstream{candidates: []*autoCandidate{
mk("llama-server", "http://127.0.0.1:8080"),
mk("ollama", "http://127.0.0.1:11434"),
}}
}

// probeUp does a cheap TCP dial to check whether a candidate is listening.
func probeUp(rawURL string) bool {
u, err := url.Parse(rawURL)
if err != nil || u.Host == "" {
return false
}
conn, err := net.DialTimeout("tcp", u.Host, autoProbeTimeout)
if err != nil {
return false
}
conn.Close()
return true
}

// pick returns the current candidate to use, re-probing at most once every
// autoRecheckInterval. It prefers staying on the current pick (if still up)
// over flapping back to a higher-priority candidate that just came back.
func (a *autoUpstream) pick() *autoCandidate {
a.mu.Lock()
defer a.mu.Unlock()
if a.current != nil && time.Since(a.checkedAt) < autoRecheckInterval {
return a.current
}
a.checkedAt = time.Now()
if a.current != nil && probeUp(a.current.url) {
return a.current
}
for _, c := range a.candidates {
if probeUp(c.url) {
a.current = c
return c
}
}
a.current = nil
return nil
}

func (a *autoUpstream) resolvedURL() string {
c := a.pick()
if c == nil {
return ""
}
return c.url
}

func (a *autoUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
c := a.pick()
if c == nil {
names := make([]string, 0, len(a.candidates))
for _, cc := range a.candidates {
names = append(names, cc.label+" ("+cc.url+")")
}
w.Header().Set("X-CsProxy", "error")
msg := "cs-proxy: ai_upstream=auto -- no backend reachable, tried: " + strings.Join(names, ", ") + "\n"
http.Error(w, msg, http.StatusBadGateway)
return
}
c.proxy.ServeHTTP(w, r)
}

// newAutoBackend wraps an autoUpstream as an aiBackend so it can sit in
// aiEdge.defaultB / aiEdge.backends exactly like a static backend.
func newAutoBackend(name, prefix, injectKey string) *aiBackend {
au := newAutoUpstream(injectKey)
return &aiBackend{name: name, prefix: prefix, upstream: "auto", proxy: au, auto: au}
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
e.backends = append(e.backends, makeBackend(n, "/"+n, cfg.AIUpstreams[n], cfg.AIUpstreamKey))
}
// ai_upstream: "" or "off" = no default backend (named backends, if any,
// still work); "auto" = dynamic llama-server-then-Ollama backend, live
// re-checked; anything else = static forward to that literal URL, exactly
// as before -- explicit manual forwarding stays available alongside auto.
mode := strings.TrimSpace(cfg.AIUpstream)
if mode != "" && !strings.EqualFold(mode, "off") {
e.defaultB = makeBackend("", "", cfg.AIUpstream, cfg.AIUpstreamKey)
}
e.setAllowed(cfg.AIAllowedIP)
return e
}

// makeBackend builds a static or dynamic backend depending on whether
// upstream is the literal "auto".
func makeBackend(name, prefix, upstream, injectKey string) *aiBackend {
if strings.EqualFold(strings.TrimSpace(upstream), "auto") {
return newAutoBackend(name, prefix, injectKey)
}
return newBackend(name, prefix, upstream, injectKey)
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
upstream := b.resolvedUpstream()
if upstream == "" {
return nil // auto backend with nothing reachable right now
}
client := &http.Client{Timeout: 5 * time.Second}
resp, err := client.Get(upstream + "/v1/models")
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
