package catalog

import (
	"context"
	"errors"
	"net/http"

	"energylens.local/cloud/internal/hubtrust"
	"github.com/go-chi/chi/v5"
)

type CertificateStore interface {
	VerifyCertificate(context.Context, string, string, int64) (hubtrust.Hub, error)
}

// HubAuthentication binds access to a currently owned, non-revoked hub. A
// prior-epoch certificate accepted for renewal cannot read catalog data.
func HubAuthentication(store CertificateStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "private, no-store")
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
				failure(w, http.StatusForbidden, "forbidden", "Forbidden")
				return
			}
			peer := r.TLS.PeerCertificates[0]
			hubID, epoch, ok := hubtrust.PeerIdentity(peer)
			if !ok || hubID != chi.URLParam(r, "hub") || peer.SerialNumber == nil {
				failure(w, http.StatusForbidden, "forbidden", "Forbidden")
				return
			}
			hub, err := store.VerifyCertificate(r.Context(), hubID, peer.SerialNumber.Text(16), epoch)
			if errors.Is(err, hubtrust.ErrDenied) {
				failure(w, http.StatusForbidden, "forbidden", "Forbidden")
				return
			}
			if err != nil {
				failure(w, http.StatusServiceUnavailable, "unavailable", "Service Unavailable")
				return
			}
			if hub.ID != hubID || hub.Epoch != epoch || hub.Revoked || hub.HomeID == nil {
				failure(w, http.StatusForbidden, "forbidden", "Forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
