package web

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"time"
)

// uiCSS is every page's stylesheet.
//
//go:embed ui.css
var uiCSS []byte

// layoutHTML holds what every page shares: the stylesheet link ("css") and the top bar ("top").
//
//go:embed layout.html
var layoutHTML string

var uiETag = func() string {
	sum := sha256.Sum256(uiCSS)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()

// stylesheet serves ui.css, revalidated on each load so a deploy's change shows at once.
func stylesheet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", uiETag)
	http.ServeContent(w, r, "ui.css", time.Time{}, bytes.NewReader(uiCSS))
}

// dictationJS is the dictation engine every page with a text box to dictate into loads.
//
//go:embed dictation.js
var dictationJS []byte

var dictationETag = func() string {
	sum := sha256.Sum256(dictationJS)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()

// dictationScript serves dictation.js, revalidated on each load like ui.css.
func dictationScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", dictationETag)
	http.ServeContent(w, r, "dictation.js", time.Time{}, bytes.NewReader(dictationJS))
}

// attachmentsJS is the image picker every form that takes a prompt loads.
//
//go:embed attachments.js
var attachmentsJS []byte

var attachmentsETag = func() string {
	sum := sha256.Sum256(attachmentsJS)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()

// attachmentsScript serves attachments.js, revalidated on each load like ui.css.
func attachmentsScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", attachmentsETag)
	http.ServeContent(w, r, "attachments.js", time.Time{}, bytes.NewReader(attachmentsJS))
}
