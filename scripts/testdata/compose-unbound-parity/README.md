# compose-unbound-parity test fixtures

Fixtures consumed by `scripts/check-compose-unbound-parity.test.sh`. Each
sub-directory is a self-contained compose bundle plus its Caddyfile,
designed to exercise one branch of `scripts/check-compose-unbound-parity.sh`.

| Fixture            | Caddyfile                | Compose state                          | Expected exit |
| ------------------ | ------------------------ | -------------------------------------- | ------------- |
| `safe/`            | no `on_demand_tls`       | no unbound service                     | `0` (no parity required) |
| `ok/`              | has `on_demand_tls`      | unbound service + `dns: ["unbound"]`   | `0`           |
| `missing-unbound/` | has `on_demand_tls`      | no unbound service                     | `1`           |
| `missing-dns/`     | has `on_demand_tls`      | unbound service, but no `dns:` pin     | `1`           |

Fixtures are sized down to the minimum keys the lint inspects. They are
not meant to be `docker compose up`-able — `docker compose config` does
treat them as syntactically valid YAML, which is what the smoke test in
the workflow asserts.
