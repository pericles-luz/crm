# `deploy/` — operator runbook

This directory holds infrastructure config that operators apply outside the
Go build: Caddy reverse-proxy, Compose stack, Prometheus alert rules, and
Grafana dashboards.

## Webhook security observability (ADR 0075 §5)

Source spec: [`docs/adr/0075-webhook-security.md`](../docs/adr/0075-webhook-security.md).
Owning task: SIN-62275.

The webhook-security gate (`webhook.security_v2.enabled = on`) is only flipped
on in prod once these two artefacts are merged and verified in staging.

### Apply Prometheus alert rules

`deploy/prometheus/webhook-rules.yml` ships five rules:

| Rule                              | Severity   | Trigger                                                                   |
| --------------------------------- | ---------- | ------------------------------------------------------------------------- |
| `WebhookSignatureInvalidRate`     | page       | `rate(webhook_received_total{outcome="signature_invalid"}[5m]) > 0.05`    |
| `WebhookUnknownTokenBurst`        | ticket     | `increase(webhook_received_total{outcome="unknown_token"}[1h]) > 10`      |
| `WebhookReplayWindowViolationRate`| page       | `rate(webhook_received_authenticated_total{outcome="replay_window_violation"}[5m]) > 0.1` |
| `WebhookUnpublishedBacklog`       | page       | `webhook_unpublished_event_count > 100` for 10m                           |
| `WebhookTenantBodyMismatchRate`   | page (F-12)| `rate(webhook_received_authenticated_total{outcome="tenant_body_mismatch"}[5m]) > 0.01` per channel/tenant |

Validate locally before applying:

```bash
promtool check rules deploy/prometheus/webhook-rules.yml
# → SUCCESS: 5 rules found
```

Apply on a self-hosted Prometheus by mounting the file and adding it to
`rule_files:` in `prometheus.yml`:

```yaml
# prometheus.yml
rule_files:
  - /etc/prometheus/rules/webhook-rules.yml
```

```bash
# example layout for the Compose stack
docker cp deploy/prometheus/webhook-rules.yml prometheus:/etc/prometheus/rules/
# reload without restart:
curl -X POST http://prometheus:9090/-/reload
```

For Prometheus Operator (kube-prometheus / GMP), wrap the rules in a
`PrometheusRule` CR (the rule bodies are unchanged — same `groups:` shape).

Tweaking a threshold is intentionally a one-line PR: change the `expr` or
`for:` field, re-run `promtool check rules`, ship.

### Import the Grafana dashboard

`deploy/grafana/webhook-security.json` panels:

1. **Outcome rate (pre-HMAC, no tenant)** — `webhook_received_total` by
   `(channel, outcome)`. Pre-HMAC outcomes by definition carry no
   `tenant_id` (ADR §2 D4 / F-9).
2. **Outcome rate (post-HMAC, authenticated)** — `webhook_received_authenticated_total`
   by `(channel, outcome)`, tenant aggregated away.
3. **Ack latency** — p50/p95/p99 of `webhook_ack_duration_seconds` per channel.
4. **Unpublished backlog** — gauge `webhook_unpublished_event_count{channel}`.
5. **Per-tenant breakdown** — only on authenticated outcomes; `tenant_id`
   templated dropdown with regex multi-select.
6. **Idempotency conflicts** — `webhook_idempotency_conflict_total` per
   channel/tenant.

The dashboard ships with two template variables:

- `DS_PROMETHEUS` — datasource picker (auto-fills from Grafana on import).
- `tenant` — multi-select tenant filter, populated from
  `label_values(webhook_received_authenticated_total, tenant_id)`. The
  default value is `.*` so the per-tenant panel shows every authenticated
  tenant until an operator narrows it.

Import via the UI:

1. Grafana → Dashboards → New → Import.
2. Upload `deploy/grafana/webhook-security.json`.
3. Pick the Prometheus datasource when prompted.

Or import via the HTTP API:

```bash
curl -sS -X POST -H "Authorization: Bearer $GRAFANA_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d @<(jq '{dashboard: ., overwrite: true, folderUid: ""}' \
        deploy/grafana/webhook-security.json) \
  https://grafana.example.com/api/dashboards/db
```

For Grafana provisioning (file-based), drop the JSON in
`/etc/grafana/provisioning/dashboards/` and reference it from a provider
YAML — no schema changes are required.

### Notes for follow-up work

- `webhook_unpublished_event_count{channel}` — gauge referenced by panel 4
  and the `WebhookUnpublishedBacklog` alert. The reconciler-side scrape
  hook is tracked separately in the SIN-62234 webhook-security epic. Until
  it is wired, panel 4 and the alert evaluate to `no data` rather than
  firing — Prometheus treats a missing series as inactive, so the alert
  is safe to land ahead of the metric.
- F-9 invariant: pre-HMAC outcomes MUST NOT carry `tenant_id`. The split
  metric layout (`webhook_received_total` vs `webhook_received_authenticated_total`)
  enforces this in the adapter; the dashboard mirrors that split panel by
  panel so an operator cannot accidentally graph an unauthenticated
  outcome with a tenant breakdown.
