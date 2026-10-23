package app

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// authCapabilities is intentionally a fixed public DTO. It does not expose
// configuration, credentials, endpoints, or provider diagnostics.
type authCapabilities struct {
	SchemaVersion              int      `json:"schema_version"`
	Methods                    []string `json:"methods"`
	RegistrationEnabled        bool     `json:"registration_enabled"`
	EmailVerificationRequired  bool     `json:"email_verification_required"`
	PasswordRecoveryEnabled    bool     `json:"password_recovery_enabled"`
	SessionLogoutRevokesAccess bool     `json:"session_logout_revokes_access"`
}

func (c Config) authCapabilitiesRoutes(r chi.Router) {
	r.Get("/auth/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		response := authCapabilities{SchemaVersion: 1, Methods: []string{}}
		switch {
		case c.RuntimeEnvironment == "local":
			response.Methods = append(response.Methods, "username_password")
			response.RegistrationEnabled = true
			response.PasswordRecoveryEnabled = true
			response.SessionLogoutRevokesAccess = true
		default:
			if c.LocalAuthEnabled {
				response.Methods = append(response.Methods, "email_password")
				response.RegistrationEnabled = true
				response.EmailVerificationRequired = false
				response.PasswordRecoveryEnabled = true
			}
			if c.OIDCIssuer != "" {
				response.Methods = append(response.Methods, "oidc")
			}
			if c.DevAuthEnabled && !c.LocalAuthEnabled {
				response.Methods = append(response.Methods, "email_password")
			}
		}
		if len(response.Methods) == 0 {
			http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	})
}
