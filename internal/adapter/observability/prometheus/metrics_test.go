package prometheus_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	pa "github.com/pericles-luz/crm/internal/adapter/observability/prometheus"
	"github.com/pericles-luz/crm/internal/webhook"
)

func gatherText(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		sb.WriteString(*mf.Name)
		sb.WriteByte('\n')
		for _, m := range mf.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				sb.WriteString("  histogram\n")
			}
			if c := m.GetCounter(); c != nil {
				sb.WriteString("  counter\n")
			}
			_ = (*dto.Metric)(m)
		}
	}
	return sb.String()
}

func TestIncReceived_RoutesByAuth(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := pa.New(reg)

	// Pre-auth: unknown token → unauth counter, no tenant label.
	m.IncReceived("whatsapp", webhook.OutcomeUnknownToken, webhook.TenantID{}, false)
	// Post-auth: accepted → authenticated counter with tenant label.
	tenant := webhook.TenantID{0xaa}
	m.IncReceived("whatsapp", webhook.OutcomeAccepted, tenant, true)

	got, err := testutil.GatherAndCount(reg, "webhook_received_total")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got != 1 {
		t.Fatalf("webhook_received_total series = %d, want 1", got)
	}

	gotAuth, err := testutil.GatherAndCount(reg, "webhook_received_authenticated_total")
	if err != nil {
		t.Fatalf("gather authed: %v", err)
	}
	if gotAuth != 1 {
		t.Fatalf("webhook_received_authenticated_total series = %d, want 1", gotAuth)
	}
}

func TestObserveAck_RecordsSeries(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := pa.New(reg)
	m.ObserveAck("whatsapp", 50*time.Millisecond)

	out := gatherText(t, reg)
	if !strings.Contains(out, "webhook_ack_duration_seconds") {
		t.Fatalf("metric not present: %s", out)
	}
}

func TestIncIdempotencyConflict_LabelsTenant(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := pa.New(reg)
	m.IncIdempotencyConflict("whatsapp", webhook.TenantID{0xaa})
	got, err := testutil.GatherAndCount(reg, "webhook_idempotency_conflict_total")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got != 1 {
		t.Fatalf("series = %d, want 1", got)
	}
}
