package sensors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestHubMappingRequiresVerifiedTLS(t *testing.T) {
	r := chi.NewRouter()
	(Service{}).HubRoutes(r)
	req := httptest.NewRequest(http.MethodGet, "/hubs/"+uuid.NewString()+"/sensors", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"forbidden"`) {
		t.Fatalf("unverified request: %d %s", w.Code, w.Body.String())
	}
}
func TestCreateRequiresCanonicalInput(t *testing.T) {
	service := Service{Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return uuid.NewString() }}
	r := chi.NewRouter()
	service.AccountRoutes(r)
	home := uuid.NewString()
	req := httptest.NewRequest(http.MethodPost, "/homes/"+home+"/sensors", strings.NewReader(`{"kind":"plug"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("invalid registration: %d %s", w.Code, w.Body.String())
	}
}
