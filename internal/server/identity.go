package server

import (
	"context"
	"net/http"
	"strings"
)

// User is the signed-in principal as reported by the authenticating reverse
// proxy. tasmonaut has no login of its own: the gpm SSO gate (Authentik
// forward auth) sits in front of it and stamps the username on every request.
// The value is display and audit only; nothing is authorised on it, because
// anyone who can reach the listener at all has already passed the gate.
type User struct {
	Username string
	Email    string
	Name     string
}

type ctxKey int

const userCtxKey ctxKey = 0

// identity reads the proxy's identity header into the request context. With no
// header (local run, curl from the pod) the user is "anonymous".
func identity(header string) func(http.Handler) http.Handler {
	emailHeader := strings.TrimSuffix(header, "-Username") + "-Email"
	nameHeader := strings.TrimSuffix(header, "-Username") + "-Name"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := User{Username: strings.TrimSpace(r.Header.Get(header))}
			if u.Username == "" {
				u.Username = "anonymous"
			} else {
				u.Email = r.Header.Get(emailHeader)
				u.Name = r.Header.Get(nameHeader)
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
		})
	}
}

// UserFromContext returns the user stored by the identity middleware.
func UserFromContext(ctx context.Context) User {
	u, _ := ctx.Value(userCtxKey).(User)
	return u
}
