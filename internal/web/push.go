package web

import (
	_ "embed"
	"errors"
	"net/http"
	"regexp"

	"github.com/zachlatta/intelligence/internal/webpush"
)

//go:embed manifest.webmanifest
var manifestJSON []byte

//go:embed sw.js
var swJS []byte

//go:embed pwa.js
var pwaJS []byte

//go:embed icon.svg
var iconSVG []byte

//go:embed icon-192.png
var icon192PNG []byte

//go:embed icon-512.png
var icon512PNG []byte

//go:embed icon-maskable-192.png
var iconMaskable192PNG []byte

//go:embed icon-maskable-512.png
var iconMaskable512PNG []byte

//go:embed apple-touch-icon.png
var appleTouchIconPNG []byte

//go:embed notification-badge.png
var notificationBadgePNG []byte

var (
	manifestAsset          = asset("manifest.webmanifest", "application/manifest+json; charset=utf-8", manifestJSON)
	pwaScript              = asset("pwa.js", "text/javascript; charset=utf-8", pwaJS)
	iconSVGAsset           = asset("icon.svg", "image/svg+xml", iconSVG)
	icon192Asset           = asset("icon-192.png", "image/png", icon192PNG)
	icon512Asset           = asset("icon-512.png", "image/png", icon512PNG)
	iconMaskable192Asset   = asset("icon-maskable-192.png", "image/png", iconMaskable192PNG)
	iconMaskable512Asset   = asset("icon-maskable-512.png", "image/png", iconMaskable512PNG)
	appleTouchIconAsset    = asset("apple-touch-icon.png", "image/png", appleTouchIconPNG)
	notificationBadgeAsset = asset("notification-badge.png", "image/png", notificationBadgePNG)
	swBaseAsset            = asset("sw.js", "text/javascript; charset=utf-8", swJS)
)

func serviceWorkerScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Service-Worker-Allowed", "/")
	swBaseAsset(w, r)
}

var pushSubIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// savePushSubscription is POST /api/push-subscriptions {"endpoint", "keys": {"p256dh", "auth"}}.
func (a *app) savePushSubscription(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.push == nil {
			http.Error(w, "push notifications are not configured", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		if !decode(w, r, &req) {
			return
		}
		id, err := a.push.Upsert(r.Context(), v.acct.ID, webpush.Subscription{
			Endpoint: req.Endpoint,
			P256dh:   req.Keys.P256dh,
			Auth:     req.Keys.Auth,
		}, r.UserAgent())
		if errors.Is(err, webpush.ErrBadSubscription) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]string{"id": id})
	}
}

// deletePushSubscriptionByEndpoint is DELETE /api/push-subscriptions {"endpoint"}.
func (a *app) deletePushSubscriptionByEndpoint(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.push == nil {
			http.Error(w, "push notifications are not configured", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Endpoint string `json:"endpoint"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.Endpoint == "" {
			http.Error(w, "endpoint is required", http.StatusBadRequest)
			return
		}
		if _, err := a.push.DeleteByEndpoint(r.Context(), v.acct.ID, req.Endpoint); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// deletePushSubscriptionByID is DELETE /api/push-subscriptions/{id}.
func (a *app) deletePushSubscriptionByID(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.push == nil {
			http.Error(w, "push notifications are not configured", http.StatusServiceUnavailable)
			return
		}
		id := r.PathValue("id")
		if !pushSubIDRe.MatchString(id) {
			http.Error(w, "subscription not found", http.StatusNotFound)
			return
		}
		deleted, err := a.push.DeleteByID(r.Context(), v.acct.ID, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !deleted {
			http.Error(w, "subscription not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// testPushNotification is POST /api/push-subscriptions/test {}.
func (a *app) testPushNotification(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.push == nil {
			http.Error(w, "push notifications are not configured", http.StatusServiceUnavailable)
			return
		}
		var req struct{}
		if !decode(w, r, &req) {
			return
		}
		sent, expired, err := a.push.SendAccount(r.Context(), v.acct.ID, webpush.Message{
			Title: "Intelligence notifications are on",
			Body:  "You will receive alerts here when a workflow asks for your reply or fails.",
			URL:   "/account",
			Tag:   "intelligence-test",
		})
		if err != nil && sent == 0 {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]int{"sent": sent, "expired": expired})
	}
}
