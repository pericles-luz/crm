package handler

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// LogoutDeleter is the slice of iam.Service the logout handler needs.
// *iam.Service satisfies it.
type LogoutDeleter interface {
	Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error
}

// Logout deletes the server-side session row (best-effort — a missing
// session is treated as already-logged-out), expires the client cookie,
// and redirects to /login. Mounted on the tenant-scoped chain but
// OUTSIDE the auth chain so users with a stale cookie can still hit it
// without a redirect loop.
func Logout(svc LogoutDeleter) http.HandlerFunc {
	if svc == nil {
		panic("handler: Logout deleter is nil")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, err := tenancy.FromContext(r.Context())
		if err != nil {
			http.Error(w, "tenant scope missing", http.StatusInternalServerError)
			return
		}
		if cookie, err := r.Cookie(middleware.SessionCookieName); err == nil && cookie.Value != "" {
			if sessionID, err := uuid.Parse(cookie.Value); err == nil {
				_ = svc.Logout(r.Context(), tenant.ID, sessionID)
			}
		}
		http.SetCookie(w, &http.Cookie{
			Name:     middleware.SessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}
