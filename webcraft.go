package main

// webcraft.go -- static serving for WebAssembly apps under /webcraft/
// (e.g. /webcraft/gridcraft/). Differences to serveStatic():
//   - directory URLs: "/webcraft/app" -> 302 "/webcraft/app/", "/webcraft/app/"
//     serves index.html (the apps load everything through relative URLs)
//   - any file type is served (.wasm, .js, .mjs, .json, ...), with the right
//     Content-Type (application/wasm is required for streaming compile)
//   - gzip for wasm/js/html/css/json: compressed ONCE per file version and kept
//     in memory (a 20 MB wasm is not recompressed per request)
//   - files are streamed with http.ServeContent (Range, If-None-Match, 304)
//   - files with a content hash in the name (trunk/wasm-bindgen style, 16 hex
//     characters) get "immutable" caching, everything else "no-cache"
//     (revalidate via ETag)
// Anything not found under /webcraft/ falls through to the normal path
// (static / webserver.pl).

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"mime"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const webcraftPrefix = "/webcraft"

// hashed file or folder name: -32ffbcd5056039fd.js, _bg.wasm after the hash,
// eframe-d31a8450ea12dfc1/inline0.js
var hashedName = regexp.MustCompile(`[-_/][0-9a-f]{16}([_./]|$)`)

var gzExt = map[string]bool{
	".wasm": true, ".js": true, ".mjs": true, ".html": true, ".htm": true,
	".css": true, ".json": true, ".svg": true, ".txt": true, ".md": true,
	".map": true, ".xml": true, ".webmanifest": true,
}

type gzEntry struct {
	data    []byte
	size    int64
	modTime time.Time
}

var (
	gzMu    sync.Mutex
	gzCache = map[string]*gzEntry{}
)

// gzFor returns the gzip form of a file, compressed once per size+mtime.
// The lock is held while compressing, so concurrent first requests do not
// compress the same file twice.
func gzFor(full string, fi os.FileInfo) *gzEntry {
	gzMu.Lock()
	defer gzMu.Unlock()
	if e, ok := gzCache[full]; ok && e.size == fi.Size() && e.modTime.Equal(fi.ModTime()) {
		return e
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		return nil
	}
	if err := gw.Close(); err != nil {
		return nil
	}
	e := &gzEntry{data: buf.Bytes(), size: fi.Size(), modTime: fi.ModTime()}
	gzCache[full] = e
	return e
}

func webcraftType(ext string) string {
	switch ext {
	case ".wasm":
		return "application/wasm"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".webmanifest":
		return "application/manifest+json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// webcraftRoots: where /webcraft/<app>/ is looked up, in this order:
//   1. <root>/_my/wwwroot   (installed by About > Webcraft Downloads, not part of the distribution)
//   2. <root>/data/wwwroot  (the docroot; older installations)
func (a *App) webcraftRoots() []string {
	my := filepath.Join(filepath.Dir(filepath.Dir(a.docroot)), "_my", "wwwroot")
	return []string{my, a.docroot}
}

// insideRoot: the resolved path (symlinks followed) must stay inside the
// resolved root.
func (a *App) insideRoot(root, full string) bool {
	res, err := filepath.EvalSymlinks(full)
	if err != nil {
		return false
	}
	rootReal := root
	if r, e := filepath.EvalSymlinks(root); e == nil {
		rootReal = r
	}
	rel, err := filepath.Rel(rootReal, res)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// webcraftList answers /webcraft/index.json: JSON array of the installed apps
// (sub folders with an index.html), alphabetically sorted. CORS-open so that
// cs-team (other port = other origin) can list them.
func (a *App) webcraftList(w http.ResponseWriter) {
	seen := map[string]bool{}
	var keys []string
	for _, rt := range a.webcraftRoots() {
		base := filepath.Join(rt, "webcraft")
		ents, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range ents {
			n := e.Name()
			if seen[n] || strings.HasPrefix(n, ".") || !a.insideRoot(rt, filepath.Join(base, n)) {
				continue
			}
			if st, err := os.Stat(filepath.Join(base, n, "index.html")); err != nil || st.IsDir() {
				continue
			}
			seen[n] = true
			keys = append(keys, n)
		}
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("[")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf("%q", k))
	}
	sb.WriteString("]")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(sb.String()))
}

// serveWebcraft serves /webcraft/...; returns false when the request is not
// for /webcraft/ or nothing was found (caller continues with the normal route).
func (a *App) serveWebcraft(w http.ResponseWriter, r *http.Request, up string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	cu := pathpkg.Clean(up) // resolves ".." -- must still be inside /webcraft
	if cu != webcraftPrefix && !strings.HasPrefix(cu, webcraftPrefix+"/") {
		return false
	}
	if cu == webcraftPrefix+"/index.json" {
		a.webcraftList(w)
		return true
	}
	var root, full string
	var fi os.FileInfo
	var err error
	for _, rt := range a.webcraftRoots() {
		f := filepath.Join(rt, filepath.FromSlash(cu))
		if !a.insideRoot(rt, f) {
			continue
		}
		if st, e := os.Stat(f); e == nil {
			root, full, fi = rt, f, st
			break
		}
	}
	if fi == nil {
		return false
	}
	if fi.IsDir() {
		if !strings.HasSuffix(up, "/") {
			loc := cu + "/"
			if r.URL.RawQuery != "" {
				loc += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, loc, http.StatusFound)
			return true
		}
		full = filepath.Join(full, "index.html")
		cu = pathpkg.Join(cu, "index.html")
		if !a.insideRoot(root, full) {
			return false
		}
		if fi, err = os.Stat(full); err != nil || fi.IsDir() {
			return false
		}
	}

	ext := strings.ToLower(filepath.Ext(full))
	h := w.Header()
	h.Set("Content-Type", webcraftType(ext))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Add("Vary", "Accept-Encoding")
	if hashedName.MatchString(cu) {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	etag := fmt.Sprintf("%x-%x", fi.Size(), fi.ModTime().UnixNano())

	// gzip: only if configured (proxy_compress), accepted by the client and
	// worth it (>= 1 KB, <= 64 MB)
	if gzExt[ext] && fi.Size() >= 1024 && fi.Size() <= 64<<20 &&
		strings.Contains(strings.ToLower(a.cfg.Compress), "gzip") &&
		strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		if e := gzFor(full, fi); e != nil {
			h.Set("Content-Encoding", "gzip")
			h.Set("ETag", `"`+etag+`-gz"`)
			if r.Header.Get("Range") == "" {
				h.Set("Content-Length", fmt.Sprintf("%d", len(e.data)))
			}
			http.ServeContent(w, r, "", e.modTime, bytes.NewReader(e.data))
			return true
		}
	}
	f, err := os.Open(full)
	if err != nil {
		return false
	}
	defer f.Close()
	h.Set("ETag", `"`+etag+`"`)
	http.ServeContent(w, r, "", fi.ModTime(), f)
	return true
}
