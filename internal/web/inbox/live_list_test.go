package inbox_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// TestListSince_ReturnsUpdatedRowsAndAdvancesCursor pins the counterpart to
// TestSince_AppendsNewBubblesAndAdvancesCursor for the conversation list: a
// row newer than the client's cursor comes back as the whole <ul> OOB-swapped
// by id, plus a fresh sentinel carrying the advanced cursor (the newest
// row's LastMessageAt UnixNano).
func TestListSince_ReturnsUpdatedRowsAndAdvancesCursor(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	newest := time.Now().UTC()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID:                 convID,
			Channel:            "whatsapp",
			State:              "open",
			ContactDisplayName: "Maria Silva",
			LastMessageSnippet: "chegou agora",
			LastMessageAt:      newest,
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/list/since?after=1", "", tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	if !summaries.called {
		t.Fatal("ListSummaries not called")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Maria Silva") || !strings.Contains(body, "chegou agora") {
		t.Errorf("missing refreshed row content: %q", body)
	}
	if !strings.Contains(body, `id="conversation-list"`) || !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("missing OOB list swap: %q", body)
	}
	wantCursor := "after=" + strconv.FormatInt(newest.UnixNano(), 10)
	if !strings.Contains(body, `id="list-live-poll"`) || !strings.Contains(body, wantCursor) {
		t.Errorf("missing advanced sentinel cursor %q in: %q", wantCursor, body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control: got %q want no-store", rec.Header().Get("Cache-Control"))
	}
}

// TestListSince_NoChangeReturns204 pins the list poll's idempotent-204
// contract, mirroring TestSince_NoChangeReturns204 for the thread poll.
func TestListSince_NoChangeReturns204(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	newest := time.Now().UTC()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{ID: uuid.New(), Channel: "whatsapp", LastMessageAt: newest}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	target := "/inbox/list/since?after=" + strconv.FormatInt(newest.UnixNano(), 10)
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, target, "", tenant))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204 body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 must carry no body, got %q", rec.Body.String())
	}
}

func TestListSince_MalformedCursorReturns400(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/list/since?after=not-a-number", "", uuid.New()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	if summaries.called {
		t.Errorf("use case must not run on a malformed cursor")
	}
}

// TestListSince_RouteAbsentWhenUnwired verifies the list live-poll route is
// not registered when ListSummaries is nil — legacy deployments keep the
// static list and a probe of the endpoint 404s at the mux, mirroring
// TestSince_RouteAbsentWhenUnwired for the thread poll.
func TestListSince_RouteAbsentWhenUnwired(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{}) // no ListSummaries
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/list/since?after=1", "", uuid.New()))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (route must be unregistered)", rec.Code)
	}
}

// TestListSince_MarksActiveRowFromQueryParam verifies the sentinel's
// ?active= param (carrying the currently open conversation) survives a
// background refresh, so the operator's open conversation stays visually
// marked (aria-current) even after the list wholesale re-renders.
func TestListSince_MarksActiveRowFromQueryParam(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	activeID := uuid.New()
	newest := time.Now().UTC()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{
			{ID: activeID, Channel: "whatsapp", LastMessageAt: newest},
			{ID: uuid.New(), Channel: "whatsapp", LastMessageAt: newest.Add(-time.Minute)},
		},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	target := "/inbox/list/since?after=1&active=" + activeID.String()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, target, "", tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "is-active") || !strings.Contains(body, `aria-current="page"`) {
		t.Errorf("active row not marked from ?active= param: %q", body)
	}
}

// TestList_RendersListPollSentinelWhenWired checks the full /inbox render
// embeds the hidden list-live-poll sentinel (seeded with the newest row's
// cursor) only when the enriched read side is wired, mirroring
// TestView_RendersLivePollSentinelWhenWired for the thread poll.
func TestList_RendersListPollSentinelWhenWired(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	newest := time.Now().UTC()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{ID: uuid.New(), Channel: "whatsapp", LastMessageAt: newest}},
	}}

	// Wired: sentinel present with the newest cursor.
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="list-live-poll"`) {
		t.Errorf("wired list missing live-poll sentinel: %q", body)
	}
	if !strings.Contains(body, "after="+strconv.FormatInt(newest.UnixNano(), 10)) {
		t.Errorf("sentinel missing seeded cursor: %q", body)
	}

	// Unwired (legacy fallback, no ListSummaries): no sentinel.
	h2 := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux2 := http.NewServeMux()
	h2.Routes(mux2)
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, reqWithTenant(http.MethodGet, "/inbox", "", tenant))
	if strings.Contains(rec2.Body.String(), `id="list-live-poll"`) {
		t.Errorf("unwired list must not render the sentinel: %q", rec2.Body.String())
	}
}
