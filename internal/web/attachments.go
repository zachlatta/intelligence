package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/zachlatta/intelligence/internal/attachment"
)

func (a *app) uploadAttachment(v viewer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
			Data []byte `json:"data"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid image upload", http.StatusBadRequest)
			return
		}
		media, err := attachment.Validate(req.Data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" || len(name) > 255 {
			http.Error(w, "image name must be 1–255 bytes", http.StatusBadRequest)
			return
		}
		var id string
		err = a.cfg.DB.QueryRow(r.Context(), `INSERT INTO attachments(account_id,name,media_type,data) VALUES($1,$2,$3,$4) RETURNING id::text`, v.acct.ID, name, media, req.Data).Scan(&id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "reference": "[attachment:" + id + "]"})
	}
}
