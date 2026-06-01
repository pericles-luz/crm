package usermfa

// SIN-63941 / UX-F4 — covers the MFA-aware credential-failure render
// path. The legacy LoginPost re-renders views.Login on
// iam.ErrInvalidCredentials, and so does usermfa.LoginPost (AC #2 —
// MFA-aware login must NOT regress the new branded layout). The
// helper now plumbs TenantName from tenancy.FromContext and the F1
// .alert--danger class flows from the shared views/login.html.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

func TestLoginPost_MFA_CredentialFailureRendersBrandedCard(t *testing.T) {
	t.Parallel()
	deps := newLoginDeps()
	deps.iam.err = iam.ErrInvalidCredentials
	h := LoginPost(deps.config())

	w := httptest.NewRecorder()
	form := url.Values{"email": []string{"alice@acme.test"}, "password": []string{"wrong"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := tenancy.WithContext(context.Background(), &tenancy.Tenant{
		ID:   uuid.New(),
		Name: "Acme Corp",
		Host: "acme.crm.local",
	})
	r = r.WithContext(ctx)

	h(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, ">Acme Corp</h1>") {
		t.Fatalf("tenant name missing on MFA credential-failure render: %q", body)
	}
	if !strings.Contains(body, `class="alert alert--danger login-card__error"`) {
		t.Fatalf("alert--danger class missing on MFA credential-failure render: %q", body)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Fatalf("role=alert missing on MFA credential-failure render: %q", body)
	}
	if !strings.Contains(body, "Email ou senha inválidos.") {
		t.Fatalf("error message body missing on MFA credential-failure render: %q", body)
	}
}

func TestLoginPost_MFA_CredentialFailureWithoutTenantStillRenders(t *testing.T) {
	t.Parallel()
	deps := newLoginDeps()
	deps.iam.err = iam.ErrInvalidCredentials
	h := LoginPost(deps.config())

	w := httptest.NewRecorder()
	form := url.Values{"email": []string{"x"}, "password": []string{"y"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	h(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, ">Entrar</h1>") {
		t.Fatalf("fallback heading missing when tenant absent: %q", body)
	}
}
