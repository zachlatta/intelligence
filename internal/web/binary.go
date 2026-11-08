package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/zachlatta/intelligence/internal/devbox"
)

var platformRe = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// binaries serves dir's intelligence-<os>-<arch> files, with their sha256, for dev box self-update.
// The files are built into the image with the server, so they are the server's version.
func binaries(dir string) http.Handler {
	var mu sync.Mutex
	sums := map[string]string{} // path -> sha256; the files do not change while the server runs
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goos, goarch := r.PathValue("os"), r.PathValue("arch")
		if dir == "" || !platformRe.MatchString(goos) || !platformRe.MatchString(goarch) {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join(dir, "intelligence-"+goos+"-"+goarch)
		f, err := os.Open(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		sum, ok := sums[path]
		if !ok {
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				mu.Unlock()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			sum = hex.EncodeToString(h.Sum(nil))
			sums[path] = sum
		}
		mu.Unlock()
		w.Header().Set(devbox.SHA256Header, sum)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "", fi.ModTime(), f)
	})
}
