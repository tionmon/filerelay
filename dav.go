package main

import (
	"log"
	"net/http"

	"golang.org/x/net/webdav"
)

// davHandler serves the files root over WebDAV at /dav/. Clients log in with
// HTTP Basic auth: any username, the token as password. A read-only token may
// browse and download but not change anything.
func (s *Server) davHandler() http.Handler {
	h := &webdav.Handler{
		Prefix:     "/dav",
		FileSystem: webdav.Dir(s.root),
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("webdav %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dav" {
			r.URL.Path = "/dav/"
		}
		a := s.requestAccess(r)
		if a == accessNone {
			w.Header().Set("WWW-Authenticate", `Basic realm="filerelay", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, "PROPFIND":
		default:
			if a < accessWrite {
				http.Error(w, "this token is read-only", http.StatusForbidden)
				return
			}
		}
		if r.Method == http.MethodPut && s.cfg.MaxUpload > 0 {
			if r.ContentLength > s.cfg.MaxUpload {
				http.Error(w, "file exceeds max upload size", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload)
		}
		h.ServeHTTP(w, r)
	})
}
