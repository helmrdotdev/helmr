package server

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/helmrdotdev/helmr/internal/console"
)

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	s.consoleAsset(w, r)
}

func (s *Server) consoleAsset(w http.ResponseWriter, r *http.Request) {
	dist := console.FS()

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name != "" && name != "." {
		if data, err := fs.ReadFile(dist, name); err == nil {
			writeConsoleFile(w, name, data, strings.HasPrefix(name, "assets/"))
			return
		}
		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}
	}

	data, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("console bundle is not available"))
		return
	}
	writeConsoleFile(w, "index.html", data, false)
}

func writeConsoleFile(w http.ResponseWriter, name string, data []byte, immutable bool) {
	w.Header().Set("content-type", consoleContentType(name, data))
	w.Header().Set("referrer-policy", "no-referrer")
	if immutable {
		w.Header().Set("cache-control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("cache-control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func consoleContentType(name string, data []byte) string {
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		return contentType
	}
	return http.DetectContentType(data)
}
