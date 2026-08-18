// Package webui serves the compiled web interface out of the binary itself.
//
// The Vite build writes into dist/, which is embedded below. That is what makes
// the server portable: a single executable carries the UI, so a deployment needs
// neither Node nor the Go toolchain. A placeholder dist/index.html is committed
// so `go build` works before the UI has ever been built; running the real build
// simply overwrites it.
package webui

import (
	"embed"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// distFS holds the built UI. The all: prefix keeps files Vite emits with a
// leading underscore or dot, which the default embed pattern would skip.
//
//go:embed all:dist
var distFS embed.FS

// indexFile is the SPA entry point, served for every unknown path so
// client-side routing works on a hard refresh.
const indexFile = "index.html"

// Handler serves the embedded UI.
func Handler() http.Handler {
	root, err := fs.Sub(distFS, "dist")
	if err != nil {
		// dist is embedded above, so this cannot happen outside a build change.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "web ui is not embedded in this build", http.StatusInternalServerError)
		})
	}
	return &handler{files: root}
}

// Built reports whether a real UI build is embedded rather than the placeholder.
// The server logs this at startup so an operator serving a stub knows why.
func Built() bool {
	body, err := distFS.ReadFile("dist/" + indexFile)
	if err != nil {
		return false
	}
	return !strings.Contains(string(body), placeholderMarker)
}

// placeholderMarker identifies the committed stub page.
const placeholderMarker = "aihub-ui-placeholder"

type handler struct {
	files fs.FS
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = indexFile
	}

	file, info, err := h.open(name)
	switch {
	case err == nil:
		defer file.Close()
	case isAssetRequest(name):
		// A missing hashed asset is a real 404: answering with index.html would
		// hand the browser HTML where it expects JavaScript, which fails with a
		// confusing MIME error instead of an honest one.
		http.NotFound(w, r)
		return
	default:
		// Any other unknown path belongs to the client-side router.
		file, info, err = h.open(indexFile)
		if err != nil {
			http.Error(w, "web ui is not built", http.StatusNotFound)
			return
		}
		defer file.Close()
		name = indexFile
	}

	seeker, ok := file.(io.ReadSeeker)
	if !ok {
		// Every embed.FS file implements io.Seeker; this is only a guard.
		http.Error(w, "cannot serve embedded file", http.StatusInternalServerError)
		return
	}

	setCacheHeaders(w, name)
	// ServeContent handles Range, If-Modified-Since and the content type. The
	// embedded modification time is zero, so it falls back to no validator,
	// which is what the cache headers above are for.
	http.ServeContent(w, r, info.Name(), info.ModTime(), seeker)
}

// open resolves a path inside the embedded tree, rejecting directories.
func (h *handler) open(name string) (fs.File, fs.FileInfo, error) {
	file, err := h.files.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if info.IsDir() {
		file.Close()
		return nil, nil, errors.New("is a directory")
	}
	return file, info, nil
}

// isAssetRequest reports whether a path looks like a build artefact rather than
// an application route.
func isAssetRequest(name string) bool {
	if strings.HasPrefix(name, "assets/") {
		return true
	}
	ext := path.Ext(name)
	return ext != "" && ext != ".html"
}

// setCacheHeaders caches hashed assets hard and the shell not at all, so a
// deployment takes effect on the next page load.
func setCacheHeaders(w http.ResponseWriter, name string) {
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	if name == indexFile {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
}
