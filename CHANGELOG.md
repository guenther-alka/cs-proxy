# cs-proxy changelog

Newest first. The section of a version is used as the text of its GitHub release
(heading format: `## vX.Y.Z`).

## v0.13.2
- New `GET /webcraft/index.json`: JSON array of the installed WebAssembly apps (sub folders of `/webcraft/` with an `index.html`), sorted alphabetically, `Access-Control-Allow-Origin: *`. Used by cs-team (Files > Single user (local)) to list all installed apps.

## v0.13.1
- Static serving for WebAssembly apps (Webcraft: GridCraft, WordCraft, PDFCraft, ...) under `/webcraft/<app>/`: `application/wasm` type (streaming compile), directory redirect and `index.html`, gzip compressed once per file version and kept in memory (for example GridCraft 21.4 MB to 8.2 MB), ETag / 304 / Range, `immutable` caching for files with a 16-hex content hash in the name, GET/HEAD only.
- Apps are looked up in `<csweb-gui>/_my/wwwroot/webcraft/` first, then in `data/wwwroot/webcraft/` (older installations); path containment checks with symlinks resolved.

## v0.12.2
- Rebuilt with Go 1.27.2 (security fixes).

## v0.12.1
- Legacy `http_port` migration and a self-reference guard that repairs `webserver.conf`.

## v0.12.0
- True `auto` mode for `ai_upstream` (llama-server, then Ollama, re-checked live).

## v0.11.1
- Fix: `proxy_compress` had no effect; https-to-http redirect downgrade fixed.

## v0.11
- AI edge hardening: key list file, path-prefix backends, GUI allowlist.

## v0.10
- AI edge: reverse proxy for local LLMs (multiple listeners).

## v0.9
- Default version string set to 0.9.
