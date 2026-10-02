package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestUploadRequiresVerifiedTLS(t *testing.T) {
	r := chi.NewRouter()
	(Service{}).HubRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/hubs/"+uuid.NewString()+"/sync/batches", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"forbidden"`) {
		t.Fatalf("unverified upload: %d %s", w.Code, w.Body.String())
	}
}
