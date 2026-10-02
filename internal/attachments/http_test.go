package attachments

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestMutationRequiresVersion(t *testing.T) {
	service := Service{Auth: func(next http.Handler) http.Handler { return next }, UserID: func(*http.Request) string { return uuid.NewString() }}
	r := chi.NewRouter()
	service.Routes(r)
	req := httptest.NewRequest(http.MethodPost, "/homes/"+uuid.NewString()+"/attachments", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", uuid.NewString())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 428 || !strings.Contains(w.Body.String(), `"code":"version_required"`) {
		t.Fatalf("missing sensor version: %d %s", w.Code, w.Body.String())
	}
}
