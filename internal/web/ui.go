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

// layoutHTML holds what every page shares: the stylesheet link and ⌘/Ctrl Enter script ("css") and
// the top bar ("top").
//
//go:embed layout.html
var layoutHTML string

// dictationJS is the dictation engine every page with a text box to dictate into loads.
//
//go:embed dictation.js
var dictationJS []byte

// submitKeysJS is the ⌘/Ctrl Enter handler every page loads with its stylesheet ("css").
//
//go:embed submitkeys.js
var submitKeysJS []byte

// attachmentsJS is the image picker every form that takes a prompt loads.
//
//go:embed attachments.js
var attachmentsJS []byte

// modelGroupsJS is the model group manager and token chips on Plans and Workflows.
//
//go:embed model-groups.js
var modelGroupsJS []byte

//go:embed model_groups.html
var modelGroupsHTML string

var (
	stylesheet        = asset("ui.css", "text/css; charset=utf-8", uiCSS)
	dictationScript   = asset("dictation.js", "text/javascript; charset=utf-8", dictationJS)
	submitKeysScript  = asset("submitkeys.js", "text/javascript; charset=utf-8", submitKeysJS)
	attachmentsScript = asset("attachments.js", "text/javascript; charset=utf-8", attachmentsJS)
	modelGroupsScript = asset("model-groups.js", "text/javascript; charset=utf-8", modelGroupsJS)
)

// asset serves an embedded file, revalidated on each load (by an ETag of its content) so a deploy's
// change shows at once.
func asset(name, contentType string, body []byte) http.HandlerFunc {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	}
}
