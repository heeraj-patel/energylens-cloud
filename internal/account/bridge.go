package account

import "net/http"

// Auth and UserID let other owner-scoped modules reuse the verified account
// identity without accepting caller-supplied identity headers.
func (s Service) Auth(next http.Handler) http.Handler { return s.auth(next) }
func UserID(r *http.Request) string                   { return user(r).ID }

// SessionID is present only for verified profiles with durable, immediately
// revocable session families. It is never accepted from an HTTP header.
func SessionID(r *http.Request) string {
	id, ok := r.Context().Value(identityKey{}).(Identity)
	if !ok {
		return ""
	}
	return id.SessionID
}
