package admin

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed web
var webFiles embed.FS

var webRoot, _ = fs.Sub(webFiles, "web")

// webTypes pins the UI's own types: mime.TypeByExtension reads the host's
// mime tables, which may map .js to something else.
var webTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
}

// serveStatic serves the embedded web UI. rest is the path after Prefix.
// The security headers are set by the caller for the whole namespace.
func serveStatic(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, errorf(http.StatusMethodNotAllowed, codeMethod, "method %s not allowed here", r.Method))
		return
	}
	if rest == "" { // relative links need the trailing slash
		http.Redirect(w, r, Prefix+"/", http.StatusMovedPermanently)
		return
	}
	name := strings.TrimPrefix(rest, "/")
	if name == "" {
		name = "index.html"
	}
	var data []byte
	err := fs.ErrNotExist
	if fs.ValidPath(name) {
		data, err = fs.ReadFile(webRoot, name)
	}
	if err != nil {
		writeError(w, errorf(http.StatusNotFound, codeNotFound, "not found"))
		return
	}
	ext := path.Ext(name)
	ctype := webTypes[ext]
	if ctype == "" {
		ctype = mime.TypeByExtension(ext)
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(data)
}
