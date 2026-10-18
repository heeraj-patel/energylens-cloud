package energycategories

import (
	"encoding/json"
	"net/http"
	"strconv"

	"energylens.local/cloud/internal/hubtrust"
	"energylens.local/cloud/internal/usagefactapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Service struct {
	Store           Store
	Auth            func(http.Handler) http.Handler
	HubCertificates hubtrust.Store
}

func (s Service) Routes(r chi.Router) { r.With(s.Auth).Get("/energy-categories", s.list) }
func (s Service) HubRoutes(r chi.Router) {
	r.With(usagefactapi.HubAuthentication(s.HubCertificates)).Get("/hubs/{hub}/energy-categories", s.export)
}
func categoryFail(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": http.StatusText(status), "message": http.StatusText(status), "request_id": uuid.NewString(), "details": map[string]any{}})
}
func notModified(w http.ResponseWriter, r *http.Request, version int64) bool {
	etag := `"` + strconv.FormatInt(version, 10) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}
func (s Service) list(w http.ResponseWriter, r *http.Request) {
	value, err := s.Store.List(r.Context())
	if err != nil {
		categoryFail(w, 500)
		return
	}
	if notModified(w, r, value.Version) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func (s Service) export(w http.ResponseWriter, r *http.Request) {
	value, version, err := s.Store.Export(r.Context())
	if err != nil {
		categoryFail(w, 500)
		return
	}
	if notModified(w, r, version) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(value, '\n'))
}
