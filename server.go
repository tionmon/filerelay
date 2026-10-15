package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html client/relay.sh
var assets embed.FS

const cookieName = "relay_token"

type access int

const (
	accessNone access = iota
	accessRead
	accessWrite
)

func (a access) String() string {
	switch a {
	case accessWrite:
		return "write"
	case accessRead:
		return "read"
	}
	return "none"
}

type Server struct {
	cfg     Config
	root    string // served files
	partial string // in-progress uploads, never visible to readers
	sums    sumCache
	busy    pathLocks
}

func NewServer(cfg Config) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		root:    filepath.Join(cfg.DataDir, "files"),
		partial: filepath.Join(cfg.DataDir, "partial"),
	}
	for _, d := range []string{s.root, s.partial} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.serveAsset("web/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /relay.sh", s.serveAsset("client/relay.sh", "text/x-shellscript; charset=utf-8"))
	mux.HandleFunc("GET /agent.md", s.handleAgentDoc)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/info", s.require(accessRead, s.handleInfo))

	for _, p := range []string{"/api/ls", "/api/ls/{path...}"} {
		mux.HandleFunc("GET "+p, s.require(accessRead, s.handleList))
	}
	mux.HandleFunc("GET /api/files/{path...}", s.require(accessRead, s.handleGet))
	mux.HandleFunc("PUT /api/files/{path...}", s.require(accessWrite, s.handlePut))
	mux.HandleFunc("POST /api/files/{path...}", s.require(accessWrite, s.handlePut))
	mux.HandleFunc("DELETE /api/files/{path...}", s.require(accessWrite, s.handleDelete))
	mux.HandleFunc("GET /api/upload-status/{path...}", s.require(accessWrite, s.handleUploadStatus))
	mux.HandleFunc("POST /api/mkdir/{path...}", s.require(accessWrite, s.handleMkdir))
	mux.HandleFunc("GET /api/sum/{path...}", s.require(accessRead, s.handleSum))
	for _, p := range []string{"/api/manifest", "/api/manifest/{path...}"} {
		mux.HandleFunc("GET "+p, s.require(accessRead, s.handleManifest))
	}
	for _, p := range []string{"/api/archive", "/api/archive/{path...}"} {
		mux.HandleFunc("GET "+p, s.require(accessRead, s.handleArchive))
	}

	dav := s.davHandler()
	mux.Handle("/dav/", dav)
	mux.Handle("/dav", dav)

	return logRequests(mux)
}

func (s *Server) serveAsset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := assets.ReadFile(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	}
}

// ---- auth ----

func (s *Server) tokenAccess(tok string) access {
	if tok == "" {
		return accessNone
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.Token)) == 1 {
		return accessWrite
	}
	if s.cfg.ReadToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.ReadToken)) == 1 {
		return accessRead
	}
	return accessNone
}

// requestAccess accepts "Authorization: Bearer <token>", HTTP Basic auth with
// the token as password (any username), or the UI's session cookie.
func (s *Server) requestAccess(r *http.Request) access {
	if h := r.Header.Get("Authorization"); h != "" {
		if t, ok := strings.CutPrefix(h, "Bearer "); ok {
			return s.tokenAccess(strings.TrimSpace(t))
		}
		if _, p, ok := r.BasicAuth(); ok {
			return s.tokenAccess(p)
		}
		return accessNone
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return s.tokenAccess(c.Value)
	}
	return accessNone
}

func (s *Server) require(min access, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch a := s.requestAccess(r); {
		case a == accessNone:
			writeErr(w, http.StatusUnauthorized, "missing or invalid token")
		case a < min:
			writeErr(w, http.StatusForbidden, "this token is read-only")
		default:
			h(w, r)
		}
	}
}

// ---- paths ----

var errBadPath = errors.New("invalid path")

// resolve maps a client path to an absolute path under the files root.
// rel is the cleaned slash path, always starting with "/".
func (s *Server) resolve(p string) (abs, rel string, err error) {
	if strings.ContainsRune(p, 0) {
		return "", "", errBadPath
	}
	rel = path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))
	if rel == "/" {
		return s.root, rel, nil
	}
	abs = filepath.Join(s.root, filepath.FromSlash(rel))
	if !strings.HasPrefix(abs, s.root+string(filepath.Separator)) {
		return "", "", errBadPath
	}
	return abs, rel, nil
}

func (s *Server) partialPath(rel string) string {
	h := sha256.Sum256([]byte(rel))
	return filepath.Join(s.partial, hex.EncodeToString(h[:16]))
}

func (s *Server) expiry(mtime time.Time) *time.Time {
	if s.cfg.TTL <= 0 {
		return nil
	}
	t := mtime.Add(s.cfg.TTL)
	return &t
}

// ---- checksum cache ----

type sumEntry struct {
	size  int64
	mtime time.Time
	sum   string
}

type sumCache struct {
	mu sync.Mutex
	m  map[string]sumEntry
}

func (c *sumCache) lookup(abs string, fi os.FileInfo) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[abs]
	if ok && e.size == fi.Size() && e.mtime.Equal(fi.ModTime()) {
		return e.sum, true
	}
	return "", false
}

func (c *sumCache) store(abs string, fi os.FileInfo, sum string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]sumEntry{}
	}
	c.m[abs] = sumEntry{fi.Size(), fi.ModTime(), sum}
}

func (c *sumCache) get(abs string, fi os.FileInfo) (string, error) {
	if sum, ok := c.lookup(abs, fi); ok {
		return sum, nil
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	c.store(abs, fi, sum)
	return sum, nil
}

// ---- per-path upload locks ----

type pathLocks struct {
	mu sync.Mutex
	m  map[string]bool
}

func (l *pathLocks) tryLock(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]bool{}
	}
	if l.m[k] {
		return false
	}
	l.m[k] = true
	return true
}

func (l *pathLocks) unlock(k string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, k)
}

// ---- expiry ----

func (s *Server) janitor(ctx context.Context) {
	if s.cfg.TTL <= 0 {
		return
	}
	every := min(max(s.cfg.TTL/20, time.Minute), 10*time.Minute)
	t := time.NewTicker(every)
	defer t.Stop()
	s.sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *Server) sweep() {
	cutoff := time.Now().Add(-s.cfg.TTL)
	var dirs []string
	emptied := map[string]bool{} // dirs that lost entries in this sweep
	removed := 0
	filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == s.root {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().Before(cutoff) {
			if os.Remove(p) == nil {
				removed++
				emptied[filepath.Dir(p)] = true
			}
		}
		return nil
	})
	// Deepest first so emptied parents can go too. os.Remove refuses non-empty
	// dirs. A dir emptied just now has a fresh mtime, so it counts as expired.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil || !(emptied[d] || fi.ModTime().Before(cutoff)) {
			continue
		}
		if os.Remove(d) == nil {
			emptied[filepath.Dir(d)] = true
		}
	}
	if ents, err := os.ReadDir(s.partial); err == nil {
		for _, e := range ents {
			if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(s.partial, e.Name()))
			}
		}
	}
	if removed > 0 {
		log.Printf("expiry: removed %d file(s) older than %v", removed, s.cfg.TTL)
	}
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func wantText(r *http.Request) bool {
	f := r.URL.Query().Get("format")
	return f == "text" || f == "tsv"
}

// ---- request logging ----

type statusWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// ReadFrom keeps sendfile(2) working for downloads behind the logger.
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.n += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		log.Printf("%s %s %s %d %dB in %v", clientIP(r), r.Method, r.URL.Path, sw.status, sw.n, time.Since(start).Round(time.Millisecond))
	})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	return r.RemoteAddr
}
