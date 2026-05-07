package validation_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/validation"
	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
)

// recordingWriter captures every LogEntry the validator emits. It also
// optionally returns errFromWrite so tests can verify the validator
// swallows writer errors (fire-and-forget contract).
type recordingWriter struct {
	mu           sync.Mutex
	entries      []validation.LogEntry
	errFromWrite error
}

func (r *recordingWriter) Write(_ context.Context, e validation.LogEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return r.errFromWrite
}

func (r *recordingWriter) only(t *testing.T) validation.LogEntry {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) != 1 {
		t.Fatalf("expected exactly one writer entry, got %d", len(r.entries))
	}
	return r.entries[0]
}

func newValidatorWithWriter(r dnsresolver.Resolver, a validation.Auditor, w validation.Writer) (*validation.Validator, time.Time) {
	t := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	return validation.New(r, a, fixedClock{t: t}, validation.WithWriter(w)), t
}

func TestWriter_HappyPath_RecordsAllowOK(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{
		{IP: ip("203.0.113.10"), VerifiedWithDNSSEC: true},
	}
	r.txtAns["_crm-verify.acme.example"] = []string{"tok"}
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, fixedT := newValidatorWithWriter(r, a, w)

	tenantID := uuid.New()
	ctx := validation.WithTenantID(context.Background(), tenantID)
	if _, err := v.Validate(ctx, "acme.example", "tok"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionAllow {
		t.Fatalf("decision = %s, want allow", got.Decision)
	}
	if got.Reason != validation.ReasonOK {
		t.Fatalf("reason = %s, want ok", got.Reason)
	}
	if got.TenantID != tenantID {
		t.Fatalf("tenant id = %s, want %s", got.TenantID, tenantID)
	}
	if got.PinnedIP != ip("203.0.113.10") {
		t.Fatalf("pinned ip = %s, want 203.0.113.10", got.PinnedIP)
	}
	if !got.VerifiedWithDNSSEC {
		t.Fatalf("dnssec flag should be true")
	}
	if got.Phase != validation.PhaseValidate {
		t.Fatalf("phase = %s, want validate", got.Phase)
	}
	if !got.At.Equal(fixedT) {
		t.Fatalf("At = %v, want %v", got.At, fixedT)
	}
}

func TestWriter_BlockedSSRF_DiscardsResolvedIP(t *testing.T) {
	// The validator MUST persist a block row WITHOUT the attacker's
	// chosen IP. Defense in depth: even if the validator forgot to
	// scrub the IP, the LogEntry it emits on the block path zeroes
	// PinnedIP, and the Postgres adapter further reinforces it by
	// writing NULL whenever decision == "block".
	r := newFakeResolver()
	r.ipAnswers["evil.example"] = []dnsresolver.IPAnswer{{IP: ip("127.0.0.1")}}
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	tenantID := uuid.New()
	ctx := validation.WithTenantID(context.Background(), tenantID)
	_, err := v.Validate(ctx, "evil.example", "tok")
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("err = %v, want ErrPrivateIP", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionBlock {
		t.Fatalf("decision = %s, want block", got.Decision)
	}
	if got.Reason != validation.ReasonPrivateIP {
		t.Fatalf("reason = %s, want private_ip", got.Reason)
	}
	if got.PinnedIP.IsValid() {
		t.Fatalf("pinned ip leaked: got %s, want zero", got.PinnedIP)
	}
	if got.TenantID != tenantID {
		t.Fatalf("tenant id missing on block row: %s", got.TenantID)
	}
}

func TestWriter_TokenMismatch_BlockReason(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtAns["_crm-verify.acme.example"] = []string{"other-token"}
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	_, err := v.Validate(context.Background(), "acme.example", "tok")
	if !errors.Is(err, validation.ErrTokenMismatch) {
		t.Fatalf("err = %v, want ErrTokenMismatch", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionBlock {
		t.Fatalf("decision = %s, want block", got.Decision)
	}
	if got.Reason != validation.ReasonTokenMismatch {
		t.Fatalf("reason = %s, want token_mismatch", got.Reason)
	}
	if got.PinnedIP.IsValid() {
		t.Fatalf("token-mismatch row must not pin an IP: %s", got.PinnedIP)
	}
}

func TestWriter_ResolverError_RecordsErrorRow(t *testing.T) {
	r := newFakeResolver()
	r.ipErrs["bork.example"] = dnsresolver.ErrTimeout
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	_, err := v.Validate(context.Background(), "bork.example", "tok")
	if !errors.Is(err, dnsresolver.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionError {
		t.Fatalf("decision = %s, want error", got.Decision)
	}
	if got.Reason != validation.ReasonResolverError {
		t.Fatalf("reason = %s, want resolver_error", got.Reason)
	}
}

func TestWriter_NoAddress_RecordsErrorRow(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["empty.example"] = []dnsresolver.IPAnswer{}
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	_, err := v.Validate(context.Background(), "empty.example", "tok")
	if !errors.Is(err, validation.ErrNoAddress) {
		t.Fatalf("err = %v, want ErrNoAddress", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionError {
		t.Fatalf("decision = %s, want error", got.Decision)
	}
	if got.Reason != validation.ReasonNoAddress {
		t.Fatalf("reason = %s, want no_address", got.Reason)
	}
}

func TestWriter_EmptyHost_RecordsEmptyInput(t *testing.T) {
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(newFakeResolver(), a, w)

	_, err := v.Validate(context.Background(), "  ", "tok")
	if !errors.Is(err, validation.ErrEmptyHost) {
		t.Fatalf("err = %v, want ErrEmptyHost", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionError || got.Reason != validation.ReasonEmptyInput {
		t.Fatalf("entry = %+v", got)
	}
}

func TestWriter_EmptyToken_RecordsEmptyInput(t *testing.T) {
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(newFakeResolver(), a, w)

	_, err := v.Validate(context.Background(), "acme.example", " ")
	if !errors.Is(err, validation.ErrEmptyToken) {
		t.Fatalf("err = %v, want ErrEmptyToken", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionError || got.Reason != validation.ReasonEmptyInput {
		t.Fatalf("entry = %+v", got)
	}
}

func TestWriter_TXTLookupError_RecordsErrorRow(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtErrs["_crm-verify.acme.example"] = errors.New("upstream blew up")
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	if _, err := v.Validate(context.Background(), "acme.example", "tok"); err == nil {
		t.Fatalf("expected error from TXT lookup")
	}
	got := w.only(t)
	if got.Decision != validation.DecisionError || got.Reason != validation.ReasonResolverError {
		t.Fatalf("entry = %+v", got)
	}
}

func TestWriter_ValidateHostOnly_AllowSuccess(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	if err := v.ValidateHostOnly(context.Background(), "acme.example"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := w.only(t)
	if got.Phase != validation.PhaseHostOnly {
		t.Fatalf("phase = %s, want host_only", got.Phase)
	}
	if got.Decision != validation.DecisionAllow || got.Reason != validation.ReasonOK {
		t.Fatalf("entry = %+v", got)
	}
	if got.PinnedIP.IsValid() {
		t.Fatalf("ValidateHostOnly must not pin an IP")
	}
}

func TestWriter_ValidateHostOnly_BlockedSSRF(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["evil.example"] = []dnsresolver.IPAnswer{{IP: ip("169.254.169.254")}}
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	if err := v.ValidateHostOnly(context.Background(), "evil.example"); !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("err = %v, want ErrPrivateIP", err)
	}
	got := w.only(t)
	if got.Phase != validation.PhaseHostOnly {
		t.Fatalf("phase = %s, want host_only", got.Phase)
	}
	if got.Decision != validation.DecisionBlock || got.Reason != validation.ReasonPrivateIP {
		t.Fatalf("entry = %+v", got)
	}
	if got.PinnedIP.IsValid() {
		t.Fatalf("blocked-ssrf row must not pin an IP")
	}
}

func TestWriter_ValidateHostOnly_NoAddress(t *testing.T) {
	r := newFakeResolver()
	r.ipAnswers["empty.example"] = []dnsresolver.IPAnswer{}
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	if err := v.ValidateHostOnly(context.Background(), "empty.example"); !errors.Is(err, validation.ErrNoAddress) {
		t.Fatalf("err = %v, want ErrNoAddress", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionError || got.Reason != validation.ReasonNoAddress {
		t.Fatalf("entry = %+v", got)
	}
}

func TestWriter_ValidateHostOnly_ResolverError(t *testing.T) {
	r := newFakeResolver()
	r.ipErrs["bork.example"] = dnsresolver.ErrTimeout
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(r, a, w)

	if err := v.ValidateHostOnly(context.Background(), "bork.example"); !errors.Is(err, dnsresolver.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionError || got.Reason != validation.ReasonResolverError {
		t.Fatalf("entry = %+v", got)
	}
}

func TestWriter_ValidateHostOnly_EmptyHost(t *testing.T) {
	a := &recordingAuditor{}
	w := &recordingWriter{}
	v, _ := newValidatorWithWriter(newFakeResolver(), a, w)

	if err := v.ValidateHostOnly(context.Background(), "  "); !errors.Is(err, validation.ErrEmptyHost) {
		t.Fatalf("err = %v, want ErrEmptyHost", err)
	}
	got := w.only(t)
	if got.Decision != validation.DecisionError || got.Reason != validation.ReasonEmptyInput {
		t.Fatalf("entry = %+v", got)
	}
}

func TestWriter_NilAuditorAndWriter_NoPanic(t *testing.T) {
	// Defensive coverage: the validator must tolerate a nil writer
	// option just like a nil Auditor, falling back to the noop default.
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtAns["_crm-verify.acme.example"] = []string{"tok"}
	v := validation.New(r, nil, fixedClock{t: time.Now()}, validation.WithWriter(nil))
	if _, err := v.Validate(context.Background(), "acme.example", "tok"); err != nil {
		t.Fatalf("nil writer must use noop fallback: %v", err)
	}
}

func TestWriter_FailureDoesNotFailValidate(t *testing.T) {
	// Writer outage MUST NOT deny a legitimate validation. The contract
	// is fire-and-forget — losing one audit row is preferable to
	// failing user-facing flows.
	r := newFakeResolver()
	r.ipAnswers["acme.example"] = []dnsresolver.IPAnswer{{IP: ip("203.0.113.10")}}
	r.txtAns["_crm-verify.acme.example"] = []string{"tok"}
	w := &recordingWriter{errFromWrite: errors.New("postgres down")}
	v, _ := newValidatorWithWriter(r, &recordingAuditor{}, w)

	if _, err := v.Validate(context.Background(), "acme.example", "tok"); err != nil {
		t.Fatalf("validate should swallow writer error; got %v", err)
	}
}

func TestTenantIDFromContext_ZeroWhenAbsent(t *testing.T) {
	if got := validation.TenantIDFromContext(context.Background()); got != uuid.Nil {
		t.Fatalf("tenant id without context = %s, want uuid.Nil", got)
	}
}

func TestTenantIDFromContext_RoundTrips(t *testing.T) {
	tenantID := uuid.New()
	ctx := validation.WithTenantID(context.Background(), tenantID)
	if got := validation.TenantIDFromContext(ctx); got != tenantID {
		t.Fatalf("tenant id round-trip = %s, want %s", got, tenantID)
	}
}

// guard against Decision/Reason vocab regressions — cheap exhaustive
// listing keeps the controlled-vocabulary contract testable.
func TestDecisionAndReason_VocabularyIsStable(t *testing.T) {
	want := []string{
		validation.DecisionAllow, validation.DecisionBlock, validation.DecisionError,
		validation.ReasonOK, validation.ReasonPrivateIP, validation.ReasonTokenMismatch,
		validation.ReasonNoAddress, validation.ReasonResolverError, validation.ReasonEmptyInput,
		validation.PhaseValidate, validation.PhaseHostOnly,
	}
	got := []string{
		"allow", "block", "error",
		"ok", "private_ip", "token_mismatch", "no_address", "resolver_error", "empty_input",
		"validate", "host_only",
	}
	if len(want) != len(got) {
		t.Fatalf("vocabulary length mismatch: %d vs %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("vocab[%d]: %q != %q", i, want[i], got[i])
		}
	}
}

// quiet unused-import warnings when only some tests above actually use a
// helper from netip — keeps the file portable when constants change.
var _ = netip.Addr{}
