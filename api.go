package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type entry struct {
	Name    string     `json:"name"`
	Path    string     `json:"path"`
	Dir     bool       `json:"dir"`
	Size    int64      `json:"size"`
	Mtime   time.Time  `json:"mtime"`
	Expires *time.Time `json:"expires,omitempty"`
}

// ---- session ----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "expected JSON {\"token\": \"...\"}")
		return
	}
	a := s.tokenAccess(strings.TrimSpace(body.Token))
	if a == accessNone {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    strings.TrimSpace(body.Token),
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"access": a.String()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return scheme + "://" + host
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	var used, files int64
	filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				used += fi.Size()
				files++
			}
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    version,
		"access":     s.requestAccess(r).String(),
		"ttlSeconds": int64(s.cfg.TTL.Seconds()),
		"maxUpload":  s.cfg.MaxUpload,
		"usedBytes":  used,
		"files":      files,
		"baseURL":    s.baseURL(r),
	})
}

// ---- browse ----

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if fi, err := os.Stat(abs); err != nil {
		statErr(w, err)
		return
	} else if !fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "not a directory")
		return
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		statErr(w, err)
		return
	}
	out := make([]entry, 0, len(ents))
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		en := entry{Name: e.Name(), Path: path.Join(rel, e.Name()), Dir: e.IsDir(), Mtime: fi.ModTime()}
		if !e.IsDir() {
			en.Size = fi.Size()
			en.Expires = s.expiry(fi.ModTime())
		}
		out = append(out, en)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	if wantText(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, e := range out {
			kind, name := "f", e.Name
			if e.Dir {
				kind, name = "d", e.Name+"/"
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", kind, e.Size, e.Mtime.UTC().Format(time.RFC3339), name)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "entries": out})
}

func statErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeErr(w, http.StatusNotFound, "not found")
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// ---- download ----

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		statErr(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		statErr(w, err)
		return
	}
	if fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "is a directory; use /api/ls, /api/manifest or /api/archive")
		return
	}
	if sum, ok := s.sums.lookup(abs, fi); ok {
		w.Header().Set("X-Checksum-Sha256", sum)
	}
	if r.URL.Query().Has("download") {
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(path.Base(rel)))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent handles Range / If-Range, so interrupted downloads resume (curl -C -, wget -c).
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// ---- upload ----

// uploadRange reads the resume offset and expected total size from either a
// Content-Range header ("bytes 100-999/1000", as sent by curl -C N -T file) or
// the ?offset= / ?total= query parameters. total is -1 when unknown.
func uploadRange(r *http.Request) (offset, total int64, err error) {
	total = -1
	if cr := r.Header.Get("Content-Range"); cr != "" {
		spec, ok := strings.CutPrefix(cr, "bytes ")
		rng, tot, ok2 := strings.Cut(spec, "/")
		start, _, ok3 := strings.Cut(rng, "-")
		if !ok || !ok2 || !ok3 {
			return 0, 0, fmt.Errorf("bad Content-Range %q", cr)
		}
		if offset, err = strconv.ParseInt(start, 10, 64); err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("bad Content-Range %q", cr)
		}
		if tot != "*" {
			if total, err = strconv.ParseInt(tot, 10, 64); err != nil || total < 0 {
				return 0, 0, fmt.Errorf("bad Content-Range %q", cr)
			}
		}
		return offset, total, nil
	}
	q := r.URL.Query()
	if v := q.Get("offset"); v != "" {
		if offset, err = strconv.ParseInt(v, 10, 64); err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("bad offset %q", v)
		}
	}
	tv := q.Get("total")
	if tv == "" {
		tv = r.Header.Get("X-Total-Size")
	}
	if tv != "" {
		if total, err = strconv.ParseInt(tv, 10, 64); err != nil || total < 0 {
			return 0, 0, fmt.Errorf("bad total %q", tv)
		}
	}
	return offset, total, nil
}

// handlePut streams the body into a staging file and atomically moves it into
// place once complete. An interrupted upload keeps its staged bytes; the client
// asks /api/upload-status for the offset and continues from there. Uploads may
// also be sent as several chunks (Content-Range with a known total): every
// chunk but the last returns 202 with the next offset.
func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if rel == "/" || strings.HasSuffix(r.PathValue("path"), "/") {
		writeErr(w, http.StatusBadRequest, "path must name a file")
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		writeErr(w, http.StatusConflict, "a directory exists at this path")
		return
	}
	offset, total, err := uploadRange(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.busy.tryLock(rel) {
		writeErr(w, http.StatusConflict, "another upload to this path is in progress")
		return
	}
	defer s.busy.unlock(rel)

	if s.cfg.MaxUpload > 0 && (offset > s.cfg.MaxUpload || total > s.cfg.MaxUpload) {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds max upload size of %d bytes", s.cfg.MaxUpload))
		return
	}

	partPath := s.partialPath(rel)
	f, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if offset > fi.Size() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "offset is beyond the bytes received so far", "offset": fi.Size()})
		return
	}
	if err := f.Truncate(offset); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Re-hash what we already have so the final checksum covers the whole file.
	h := sha256.New()
	if offset > 0 {
		if _, err := io.CopyN(h, f, offset); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var body io.Reader = r.Body
	if s.cfg.MaxUpload > 0 {
		body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload-offset)
	}
	n, err := io.CopyBuffer(io.MultiWriter(f, h), body, make([]byte, 1<<20))
	size := offset + n
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			f.Close()
			closed = true
			os.Remove(partPath)
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds max upload size of %d bytes", s.cfg.MaxUpload))
			return
		}
		f.Sync()
		log.Printf("upload %s interrupted at %d bytes: %v", rel, size, err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upload interrupted: " + err.Error(), "offset": size})
		return
	}
	if total >= 0 && size < total {
		f.Sync()
		writeJSON(w, http.StatusAccepted, map[string]any{"path": rel, "offset": size, "total": total, "complete": false})
		return
	}
	if total >= 0 && size > total {
		f.Close()
		closed = true
		os.Remove(partPath)
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("received %d bytes but total was declared as %d", size, total))
		return
	}
	if err := f.Sync(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	f.Close()
	closed = true

	sum := hex.EncodeToString(h.Sum(nil))
	want := r.Header.Get("X-Checksum-Sha256")
	if want == "" {
		want = r.URL.Query().Get("sha256")
	}
	if want != "" && !strings.EqualFold(strings.TrimSpace(want), sum) {
		os.Remove(partPath)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "checksum mismatch; upload discarded, start again from offset 0", "sha256": sum, "expected": want})
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		writeErr(w, http.StatusConflict, "cannot create parent directory: "+err.Error())
		return
	}
	err = os.Rename(partPath, abs)
	if errors.Is(err, fs.ErrNotExist) {
		// The expiry sweep may have removed the (empty) parent in between.
		if err = os.MkdirAll(filepath.Dir(abs), 0o755); err == nil {
			err = os.Rename(partPath, abs)
		}
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	os.Chtimes(abs, now, now) // expiry counts from upload completion
	if fi, err := os.Stat(abs); err == nil {
		s.sums.store(abs, fi, sum)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"path": rel, "size": size, "sha256": sum, "complete": true, "expires": s.expiry(now)})
}

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var offset int64
	if fi, err := os.Stat(s.partialPath(rel)); err == nil {
		offset = fi.Size()
	}
	if wantText(r) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, offset)
		return
	}
	res := map[string]any{"path": rel, "offset": offset, "exists": false}
	if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
		res["exists"] = true
		res["size"] = fi.Size()
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- modify ----

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if rel == "/" {
		writeErr(w, http.StatusBadRequest, "refusing to delete the root")
		return
	}
	os.Remove(s.partialPath(rel))
	if _, err := os.Lstat(abs); err != nil {
		statErr(w, err)
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": rel})
}

// ---- verify / bulk ----

func (s *Server) handleSum(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		statErr(w, err)
		return
	}
	if fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "is a directory; use /api/manifest?sha256=1")
		return
	}
	sum, err := s.sums.get(abs, fi)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wantText(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s  %s\n", sum, path.Base(rel)) // sha256sum format
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "size": fi.Size(), "sha256": sum})
}

type manifestEntry struct {
	Path   string `json:"path"` // relative to the parent of the requested path
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// handleManifest lists every file under a path, recursively. Paths in the
// result are relative to the requested path's parent, so a client can recreate
// "<dest>/<name>/..." for directories and "<dest>/<name>" for single files.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(abs); err != nil {
		statErr(w, err)
		return
	}
	withSum := r.URL.Query().Get("sha256") == "1"
	base := filepath.Dir(abs)
	if rel == "/" {
		base = abs
	}
	var out []manifestEntry
	var total int64
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		relp, _ := filepath.Rel(base, p)
		e := manifestEntry{Path: filepath.ToSlash(relp), Size: fi.Size()}
		if withSum {
			if e.SHA256, err = s.sums.get(p, fi); err != nil {
				return err
			}
		}
		total += fi.Size()
		out = append(out, e)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wantText(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, e := range out {
			sum := e.SHA256
			if sum == "" {
				sum = "-"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\n", e.Size, sum, e.Path)
		}
		return
	}
	if out == nil {
		out = []manifestEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "files": out, "count": len(out), "totalBytes": total})
}

// handleArchive streams a directory (or file) as tar.gz, or plain tar with ?gzip=0.
// Handy for one-shot pulls; for unreliable links prefer per-file downloads,
// which can resume.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := s.resolve(r.PathValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(abs); err != nil {
		statErr(w, err)
		return
	}
	name := path.Base(rel)
	base := filepath.Dir(abs)
	if rel == "/" {
		name, base = "relay", abs
	}
	gz := r.URL.Query().Get("gzip") != "0"
	fname := name + ".tar"
	if gz {
		fname += ".gz"
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(fname))

	var out io.Writer = w
	var zw *gzip.Writer
	if gz {
		zw, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
		out = zw
	}
	tw := tar.NewWriter(out)
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == base {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		relp, _ := filepath.Rel(base, p)
		hdr.Name = filepath.ToSlash(relp)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil && zw != nil {
		err = zw.Close()
	}
	if err != nil {
		// Headers are already sent. Abort the connection without writing the
		// tar/gzip trailers so the client sees a failed, not a short, archive.
		log.Printf("archive %s: %v", rel, err)
		panic(http.ErrAbortHandler)
	}
}
