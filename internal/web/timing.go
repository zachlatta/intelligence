package web

import (
	"context"
	_ "embed"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zachlatta/intelligence/internal/timing"
)

// Every request is timed (http_requests) and every signed-in page reports its own load timing
// (pageload.js, POST /api/page-loads, page_loads): internal/timing.

//go:embed pageload.js
var pageLoadJS []byte

var pageLoadScript = asset("pageload.js", "text/javascript; charset=utf-8", pageLoadJS)

// noRoute is the route of a request no pattern matched.
const noRoute = "(no route)"

// requestAccount carries the account a request is signed in as from wherever it is
// authenticated (viewerOf, hostCaller, workflowAPI) out to timed.
type requestAccount struct{ id atomic.Int64 }

type requestAccountKey struct{}

// signedInAs notes that r is from account id, for its timing row.
func signedInAs(r *http.Request, id int64) {
	if a, ok := r.Context().Value(requestAccountKey{}).(*requestAccount); ok {
		a.id.Store(id)
	}
}

// timed serves mux, recording each request's route pattern, method, status and duration to rec.
// Nothing else about the request (path, query, headers, body) is recorded.
func timed(rec *timing.Recorder, now func() time.Time, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := now()
		acct := &requestAccount{}
		r = r.WithContext(context.WithValue(r.Context(), requestAccountKey{}, acct))
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			if p := recover(); p != nil {
				status = http.StatusInternalServerError
				defer panic(p)
			}
			// the mux sets r.Pattern on the request it was given, which is r
			rec.Request(timing.Request{At: start, AccountID: acct.id.Load(), Method: r.Method,
				Route: routeOf(r.Pattern), Status: status, Duration: now().Sub(start)})
		}()
		mux.ServeHTTP(sw, r)
	})
}

// routeOf is pattern without its method: "GET /workflows/{id}" is "/workflows/{id}".
func routeOf(pattern string) string {
	if pattern == "" {
		return noRoute
	}
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

// statusWriter notes the status a handler answers with. It passes Flush on (the SSE streams
// need it) and unwraps for http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
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
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// maxPageLoadMS bounds each reported timing: ten minutes. Longer is clamped to it.
const maxPageLoadMS = 600000

// navigationTypes are the values PerformanceNavigationTiming.type can take; any other is refused.
var navigationTypes = map[string]bool{"navigate": true, "reload": true, "back_forward": true, "prerender": true}

type pageLoadReport struct {
	Path               string  `json:"path"`
	NavigationType     string  `json:"navigationType"`
	TTFBMS             float64 `json:"ttfbMs"`
	DOMContentLoadedMS float64 `json:"domContentLoadedMs"`
	LoadMS             float64 `json:"loadMs"`
	DurationMS         float64 `json:"durationMs"`
}

// recordPageLoad stores a page's own navigation timing (POST /api/page-loads), under the route
// pattern that serves its path; the path itself is not stored.
func (a *app) recordPageLoad(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p pageLoadReport
		if !decode(w, r, &p) {
			return
		}
		if !strings.HasPrefix(p.Path, "/") || strings.HasPrefix(p.Path, "//") {
			http.Error(w, "path must be the page's path", http.StatusBadRequest)
			return
		}
		if !navigationTypes[p.NavigationType] {
			http.Error(w, "navigationType must be navigate, reload, back_forward or prerender", http.StatusBadRequest)
			return
		}
		ms := []*float64{&p.TTFBMS, &p.DOMContentLoadedMS, &p.LoadMS, &p.DurationMS}
		for _, m := range ms {
			if math.IsNaN(*m) || *m < 0 {
				http.Error(w, "timings must be milliseconds, 0 or more", http.StatusBadRequest)
				return
			}
			*m = min(*m, maxPageLoadMS)
		}
		u, err := url.Parse(p.Path)
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		_, pattern := a.routes.Handler(&http.Request{Method: http.MethodGet, URL: &url.URL{Path: u.Path}, Host: r.Host})
		err = timing.RecordPageLoad(r.Context(), a.cfg.DB, timing.PageLoad{At: a.cfg.Now(), AccountID: v.acct.ID,
			Route: routeOf(pattern), NavigationType: p.NavigationType,
			TTFBMS: p.TTFBMS, DOMContentLoadedMS: p.DOMContentLoadedMS, LoadMS: p.LoadMS, DurationMS: p.DurationMS})
		if err != nil {
			log.Print(err)
			http.Error(w, "could not record the page load", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
