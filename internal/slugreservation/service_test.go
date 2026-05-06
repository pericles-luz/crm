package slugreservation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/slugreservation"
)

// fakeStore is an in-memory Store for unit tests. It mirrors the
// partial-unique-index semantics: at most one active reservation per
// slug.
type fakeStore struct {
	rows      map[string]slugreservation.Reservation
	insertErr error
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]slugreservation.Reservation{}} }

func (s *fakeStore) Active(_ context.Context, slug string) (slugreservation.Reservation, error) {
	res, ok := s.rows[slug]
	if !ok || !res.ExpiresAt.After(time.Now()) {
		return slugreservation.Reservation{}, slugreservation.ErrNotReserved
	}
	return res, nil
}

func (s *fakeStore) Insert(_ context.Context, slug string, by uuid.UUID, releasedAt, expiresAt time.Time) (slugreservation.Reservation, error) {
	if s.insertErr != nil {
		return slugreservation.Reservation{}, s.insertErr
	}
	if existing, ok := s.rows[slug]; ok && existing.ExpiresAt.After(time.Now()) {
		return slugreservation.Reservation{}, &slugreservation.ReservedError{Reservation: existing}
	}
	r := slugreservation.Reservation{
		ID:                 uuid.New(),
		Slug:               slug,
		ReleasedAt:         releasedAt,
		ReleasedByTenantID: by,
		ExpiresAt:          expiresAt,
		CreatedAt:          releasedAt,
	}
	s.rows[slug] = r
	return r, nil
}

func (s *fakeStore) SoftDelete(_ context.Context, slug string, at time.Time) (slugreservation.Reservation, error) {
	r, ok := s.rows[slug]
	if !ok || !r.ExpiresAt.After(at) {
		return slugreservation.Reservation{}, slugreservation.ErrNotReserved
	}
	r.ExpiresAt = at
	s.rows[slug] = r
	return r, nil
}

type fakeRedirectStore struct {
	rows map[string]slugreservation.Redirect
}

func newFakeRedirectStore() *fakeRedirectStore {
	return &fakeRedirectStore{rows: map[string]slugreservation.Redirect{}}
}

func (s *fakeRedirectStore) Active(_ context.Context, slug string) (slugreservation.Redirect, error) {
	r, ok := s.rows[slug]
	if !ok || !r.ExpiresAt.After(time.Now()) {
		return slugreservation.Redirect{}, slugreservation.ErrNotReserved
	}
	return r, nil
}

func (s *fakeRedirectStore) Upsert(_ context.Context, oldSlug, newSlug string, exp time.Time) (slugreservation.Redirect, error) {
	r := slugreservation.Redirect{OldSlug: oldSlug, NewSlug: newSlug, ExpiresAt: exp}
	s.rows[oldSlug] = r
	return r, nil
}

type fakeAudit struct {
	calls []slugreservation.MasterOverrideEvent
	err   error
}

func (a *fakeAudit) LogMasterOverride(_ context.Context, ev slugreservation.MasterOverrideEvent) error {
	if a.err != nil {
		return a.err
	}
	a.calls = append(a.calls, ev)
	return nil
}

type fakeSlack struct {
	msgs []string
	err  error
}

func (s *fakeSlack) NotifyAlert(_ context.Context, msg string) error {
	if s.err != nil {
		return s.err
	}
	s.msgs = append(s.msgs, msg)
	return nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func newSvc(t *testing.T, now time.Time) (*slugreservation.Service, *fakeStore, *fakeRedirectStore, *fakeAudit, *fakeSlack) {
	t.Helper()
	store := newFakeStore()
	red := newFakeRedirectStore()
	audit := &fakeAudit{}
	slack := &fakeSlack{}
	svc, err := slugreservation.NewService(store, red, audit, slack, fixedClock{t: now})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store, red, audit, slack
}

func TestNewService_RequiresPorts(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	red := newFakeRedirectStore()
	audit := &fakeAudit{}
	slack := &fakeSlack{}

	cases := []struct {
		name  string
		store slugreservation.Store
		red   slugreservation.RedirectStore
		audit slugreservation.MasterAuditLogger
		slack slugreservation.SlackNotifier
	}{
		{"nil store", nil, red, audit, slack},
		{"nil redirects", store, nil, audit, slack},
		{"nil audit", store, red, nil, slack},
		{"nil slack", store, red, audit, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := slugreservation.NewService(tc.store, tc.red, tc.audit, tc.slack, nil); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}

	if _, err := slugreservation.NewService(store, red, audit, slack, nil); err != nil {
		t.Fatalf("expected nil clock to default, got %v", err)
	}
}

func TestCheckAvailable_Empty(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC))
	if err := svc.CheckAvailable(context.Background(), "acme"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckAvailable_Reserved(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	svc, store, _, _, _ := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{
		Slug:      "acme",
		ExpiresAt: now.Add(slugreservation.ReservationWindow),
	}
	err := svc.CheckAvailable(context.Background(), "ACME")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, slugreservation.ErrSlugReserved) {
		t.Fatalf("expected ErrSlugReserved, got %v", err)
	}
	var rerr *slugreservation.ReservedError
	if !errors.As(err, &rerr) {
		t.Fatal("expected *ReservedError")
	}
	if rerr.Reservation.Slug != "acme" {
		t.Fatalf("slug=%q", rerr.Reservation.Slug)
	}
	if !strings.Contains(rerr.Error(), "reserved until") {
		t.Fatalf("error msg=%q", rerr.Error())
	}
}

func TestCheckAvailable_InvalidSlug(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	cases := []string{"", "  ", "-bad", "bad-", "BAD!", strings.Repeat("a", 64)}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			err := svc.CheckAvailable(context.Background(), in)
			if !errors.Is(err, slugreservation.ErrInvalidSlug) {
				t.Fatalf("input=%q err=%v", in, err)
			}
		})
	}
}

type errStore struct{ slugreservation.Store }

func (errStore) Active(context.Context, string) (slugreservation.Reservation, error) {
	return slugreservation.Reservation{}, errors.New("boom")
}

func TestCheckAvailable_StoreError(t *testing.T) {
	t.Parallel()
	red := newFakeRedirectStore()
	audit := &fakeAudit{}
	slack := &fakeSlack{}
	svc, err := slugreservation.NewService(errStore{}, red, audit, slack, fixedClock{t: time.Now()})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.CheckAvailable(context.Background(), "acme"); err == nil {
		t.Fatal("expected error")
	} else if errors.Is(err, slugreservation.ErrSlugReserved) {
		t.Fatal("did not expect ErrSlugReserved")
	}
}

func TestReleaseSlug_Window(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, store, _, _, _ := newSvc(t, now)
	tenantID := uuid.New()
	res, err := svc.ReleaseSlug(context.Background(), "Acme", tenantID)
	if err != nil {
		t.Fatalf("ReleaseSlug: %v", err)
	}
	if res.Slug != "acme" {
		t.Fatalf("slug=%q", res.Slug)
	}
	want := now.Add(slugreservation.ReservationWindow)
	if !res.ExpiresAt.Equal(want) {
		t.Fatalf("expires=%v want=%v", res.ExpiresAt, want)
	}
	if res.ReleasedByTenantID != tenantID {
		t.Fatalf("releasedBy=%v want=%v", res.ReleasedByTenantID, tenantID)
	}
	if _, ok := store.rows["acme"]; !ok {
		t.Fatal("row not stored")
	}
}

func TestReleaseSlug_InvalidSlug(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	if _, err := svc.ReleaseSlug(context.Background(), "BAD!", uuid.New()); !errors.Is(err, slugreservation.ErrInvalidSlug) {
		t.Fatalf("err=%v", err)
	}
}

func TestReleaseSlug_StoreError(t *testing.T) {
	t.Parallel()
	now := time.Now()
	svc, store, _, _, _ := newSvc(t, now)
	store.insertErr = errors.New("db down")
	if _, err := svc.ReleaseSlug(context.Background(), "acme", uuid.New()); err == nil {
		t.Fatal("expected error")
	}
}

func TestOverrideRelease_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, store, _, audit, slack := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{
		Slug:      "acme",
		ExpiresAt: now.Add(slugreservation.ReservationWindow),
	}
	masterID := uuid.New()
	res, err := svc.OverrideRelease(context.Background(), "acme", masterID, "incident #42")
	if err != nil {
		t.Fatalf("OverrideRelease: %v", err)
	}
	if !res.ExpiresAt.Equal(now) {
		t.Fatalf("expires=%v want=%v", res.ExpiresAt, now)
	}
	if len(audit.calls) != 1 {
		t.Fatalf("audit calls=%d", len(audit.calls))
	}
	got := audit.calls[0]
	if got.Slug != "acme" || got.MasterID != masterID || got.Reason != "incident #42" {
		t.Fatalf("audit=%+v", got)
	}
	if !got.At.Equal(now) {
		t.Fatalf("audit at=%v want=%v", got.At, now)
	}
	if len(slack.msgs) != 1 {
		t.Fatalf("slack msgs=%d", len(slack.msgs))
	}
	if !strings.Contains(slack.msgs[0], "acme") || !strings.Contains(slack.msgs[0], "incident #42") {
		t.Fatalf("slack msg=%q", slack.msgs[0])
	}
}

func TestOverrideRelease_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		slug    string
		master  uuid.UUID
		reason  string
		wantErr error
	}{
		{"zero master", "acme", uuid.Nil, "x", slugreservation.ErrZeroMaster},
		{"empty reason", "acme", uuid.New(), "  ", slugreservation.ErrReasonRequired},
		{"invalid slug", "BAD!", uuid.New(), "x", slugreservation.ErrInvalidSlug},
		{"not reserved", "other", uuid.New(), "x", slugreservation.ErrNotReserved},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
			svc, store, _, _, _ := newSvc(t, now)
			store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}
			_, err := svc.OverrideRelease(context.Background(), tc.slug, tc.master, tc.reason)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v want=%v", err, tc.wantErr)
			}
		})
	}
}

func TestOverrideRelease_AuditFailureHaltsOverride(t *testing.T) {
	t.Parallel()
	now := time.Now()
	svc, store, _, audit, slack := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}
	audit.err = errors.New("audit down")

	if _, err := svc.OverrideRelease(context.Background(), "acme", uuid.New(), "reason"); err == nil {
		t.Fatal("expected audit failure to bubble")
	}
	if len(slack.msgs) != 0 {
		t.Fatal("slack should not fire when audit fails")
	}
}

func TestOverrideRelease_SlackFailureNonFatal(t *testing.T) {
	t.Parallel()
	now := time.Now()
	svc, store, _, audit, slack := newSvc(t, now)
	store.rows["acme"] = slugreservation.Reservation{Slug: "acme", ExpiresAt: now.Add(time.Hour)}
	slack.err = errors.New("slack 500")
	if _, err := svc.OverrideRelease(context.Background(), "acme", uuid.New(), "reason"); err != nil {
		t.Fatalf("override should swallow slack error: %v", err)
	}
	if len(audit.calls) != 1 {
		t.Fatalf("audit calls=%d", len(audit.calls))
	}
}

func TestLookupRedirect(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, _, red, _, _ := newSvc(t, now)
	red.rows["acme"] = slugreservation.Redirect{OldSlug: "acme", NewSlug: "acme-2", ExpiresAt: now.Add(time.Hour)}
	got, err := svc.LookupRedirect(context.Background(), "acme")
	if err != nil {
		t.Fatalf("LookupRedirect: %v", err)
	}
	if got.NewSlug != "acme-2" {
		t.Fatalf("new=%q", got.NewSlug)
	}
}

func TestLookupRedirect_InvalidSlug(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	if _, err := svc.LookupRedirect(context.Background(), "BAD!"); !errors.Is(err, slugreservation.ErrInvalidSlug) {
		t.Fatalf("err=%v", err)
	}
}

func TestLookupRedirect_NotFound(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newSvc(t, time.Now())
	if _, err := svc.LookupRedirect(context.Background(), "acme"); !errors.Is(err, slugreservation.ErrNotReserved) {
		t.Fatalf("err=%v", err)
	}
}

func TestRecordRedirect(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, _, red, _, _ := newSvc(t, now)
	got, err := svc.RecordRedirect(context.Background(), "Acme", "acme-2")
	if err != nil {
		t.Fatalf("RecordRedirect: %v", err)
	}
	want := now.Add(slugreservation.RedirectWindow)
	if !got.ExpiresAt.Equal(want) {
		t.Fatalf("expires=%v want=%v", got.ExpiresAt, want)
	}
	if red.rows["acme"].NewSlug != "acme-2" {
		t.Fatalf("redirect rows=%+v", red.rows)
	}
}

func TestRecordRedirect_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		old, new string
	}{
		{"old invalid", "BAD!", "acme-2"},
		{"new invalid", "acme", "BAD!"},
		{"same", "acme", "acme"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _, _, _, _ := newSvc(t, time.Now())
			if _, err := svc.RecordRedirect(context.Background(), tc.old, tc.new); !errors.Is(err, slugreservation.ErrInvalidSlug) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

type errRedirect struct{ slugreservation.RedirectStore }

func (errRedirect) Active(context.Context, string) (slugreservation.Redirect, error) {
	return slugreservation.Redirect{}, errors.New("boom")
}
func (errRedirect) Upsert(context.Context, string, string, time.Time) (slugreservation.Redirect, error) {
	return slugreservation.Redirect{}, errors.New("boom")
}

func TestRedirectStoreErrors(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	red := errRedirect{}
	audit := &fakeAudit{}
	slack := &fakeSlack{}
	svc, err := slugreservation.NewService(store, red, audit, slack, fixedClock{t: time.Now()})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.LookupRedirect(context.Background(), "acme"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := svc.RecordRedirect(context.Background(), "acme", "acme-2"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, out string
		ok      bool
	}{
		{"acme", "acme", true},
		{"  Acme ", "acme", true},
		{"acme-2", "acme-2", true},
		{"a", "a", true},
		{"", "", false},
		{"-bad", "", false},
		{"bad-", "", false},
		{"BAD!", "", false},
		{strings.Repeat("a", 64), "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			got, err := slugreservation.NormalizeSlug(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("err=%v", err)
				}
				if got != tc.out {
					t.Fatalf("got=%q want=%q", got, tc.out)
				}
			} else if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
		})
	}
}

func TestSystemClock(t *testing.T) {
	t.Parallel()
	c := slugreservation.SystemClock{}
	now := c.Now()
	if now.IsZero() {
		t.Fatal("zero time")
	}
	if now.Location() != time.UTC {
		t.Fatalf("loc=%v", now.Location())
	}
}

func TestServiceNow(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	svc, _, _, _, _ := newSvc(t, at)
	if !svc.Now().Equal(at) {
		t.Fatalf("now=%v want=%v", svc.Now(), at)
	}
}

func TestFormatExpiresAt(t *testing.T) {
	t.Parallel()
	got := slugreservation.FormatExpiresAt(time.Date(2027, 5, 6, 12, 0, 0, 0, time.UTC))
	if got != "2027-05-06" {
		t.Fatalf("got=%q", got)
	}
}
