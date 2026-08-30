package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// static extensions that cs-proxy serves directly from the docroot.
var staticExt = map[string]bool{
	".css": true, ".png": true, ".js": true, ".ico": true, ".svg": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".jpg": true,
	".jpeg": true, ".gif": true, ".bmp": true, ".webp": true, ".txt": true,
	".json": true, ".xml": true, ".map": true, ".pdf": true, ".html": true,
}

type cacheEntry struct {
	data      []byte
	etag      string
	ctype     string
	modTime   time.Time
	storedAt  time.Time
}

type StaticCache struct {
	mu       sync.Mutex
	m        map[string]*cacheEntry
	maxBytes int64
	ttl      time.Duration
	total    int64
	order    []string
}

func newStaticCache(maxBytes int64, ttlSec int) *StaticCache {
	if maxBytes <= 0 {
		maxBytes = 64 * 1024 * 1024
	}
	if ttlSec <= 0 {
		ttlSec = 60
	}
	return &StaticCache{m: map[string]*cacheEntry{}, maxBytes: maxBytes, ttl: time.Duration(ttlSec) * time.Second}
}

func (c *StaticCache) get(key string) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.storedAt) > c.ttl {
		c.removeLocked(key)
		return nil, false
	}
	return e, true
}

func (c *StaticCache) put(key string, e *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.m[key]; ok {
		c.total -= int64(len(old.data))
	}
	c.m[key] = e
	c.total += int64(len(e.data))
	c.order = append(c.order, key)
	// evict expired, then oldest until under budget
	now := time.Now()
	for i := 0; i < len(c.order); {
		k := c.order[i]
		if ent, ok := c.m[k]; ok && now.Sub(ent.storedAt) > c.ttl {
			c.removeLocked(k)
			c.order = append(c.order[:i], c.order[i+1:]...)
		} else {
			i++
		}
	}
	for c.total > c.maxBytes && len(c.order) > 0 {
		k := c.order[0]
		c.removeLocked(k)
		c.order = c.order[1:]
	}
}

func (c *StaticCache) removeLocked(key string) {
	if e, ok := c.m[key]; ok {
		c.total -= int64(len(e.data))
		delete(c.m, key)
	}
}

// serveStatic serves a file from the docroot; returns false when not found
// (caller then reverse-proxies the request).
func (a *App) serveStatic(w http.ResponseWriter, r *http.Request, path string) bool {
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return false // traversal
	}
	ext := strings.ToLower(filepath.Ext(clean))
	if !staticExt[ext] && !strings.HasPrefix(path, "/_doc/") && !strings.HasPrefix(path, "/_my/") {
		return false // only static-looking paths are served here
	}
	full := filepath.Join(a.docroot, clean)
	// containment check (URL paths start with "/", so a naive IsAbs test is
	// wrong on POSIX -- verify the joined path stays inside the docroot).
	if rel, err := filepath.Rel(a.docroot, full); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	// symlink hardening (B5): os.Stat below follows symlinks, so re-check the
	// RESOLVED path stays inside the (resolved) docroot. Broken symlinks and
	// symlinks escaping the docroot are rejected.
	if res, err := filepath.EvalSymlinks(full); err != nil {
		return false
	} else if rel, err := filepath.Rel(a.docrootReal, res); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		return false
	}
	noCache := noCacheRequest(r)

	key := path + "?" + r.URL.RawQuery
	if !noCache {
		if e, ok := a.cache.get(key); ok {
			serveFromCache(w, r, e)
			return true
		}
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	if strings.HasSuffix(path, ".svg") {
		ctype = "image/svg+xml"
	}
	e := &cacheEntry{data: data, etag: etag, ctype: ctype, modTime: fi.ModTime(), storedAt: time.Now()}
	if !noCache && a.cfg.CacheOn {
		a.cache.put(key, e)
	}
	serveFromCache(w, r, e)
	return true
}

func noCacheRequest(r *http.Request) bool {
	cc := r.Header.Get("Cache-Control")
	p := r.Header.Get("Pragma")
	if strings.Contains(strings.ToLower(cc), "no-cache") || strings.Contains(strings.ToLower(p), "no-cache") {
		return true
	}
	if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
		return true // conditional request -> always serve fresh + 304 path
	}
	return false
}

func serveFromCache(w http.ResponseWriter, r *http.Request, e *cacheEntry) {
	if r.Header.Get("If-None-Match") == e.etag {
		w.Header().Set("ETag", e.etag)
		w.Header().Set("Cache-Control", "public, max-age="+fmt.Sprintf("%d", 0))
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", e.etag)
	w.Header().Set("Content-Type", e.ctype)
	w.Header().Set("Last-Modified", e.modTime.UTC().Format(http.TimeFormat))
	w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
	gz := wantsGzip(r, e.ctype)
	if gz {
		w.Header().Set("Content-Encoding", "gzip")
		gw := gzip.NewWriter(w)
		gw.Write(e.data)
		gw.Close()
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(e.data)))
	io.WriteString(w, string(e.data))
}

func wantsGzip(r *http.Request, ctype string) bool {
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		return false
	}
	ct := strings.ToLower(ctype)
	if strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") ||
		strings.Contains(ct, "javascript") || strings.Contains(ct, "xml") ||
		strings.Contains(ct, "svg") {
		return true
	}
	return false
}
