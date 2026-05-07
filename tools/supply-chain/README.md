# tools/supply-chain

Regression tests for the supply-chain baseline introduced in
[ADR 0084](../../docs/adr/0084-supply-chain.md) ([SIN-62247](/SIN/issues/SIN-62247)).

| Script | What it covers | External tools needed |
| --- | --- | --- |
| `test_deploy_gate.sh` | `deploy/scripts/stg-deploy.sh` refuses to call `docker compose pull/up` when `cosign verify` fails (and proceeds when it succeeds). Uses fake `docker` and `cosign` on `PATH`. | bash only |
| `test_workflow_invariants.sh` | The cosign sign step in `cd-stg.yml`, the SBOM steps, the `--certificate-identity-regexp` binding in `stg-deploy.sh`, the `>= high` Dependabot-alert gate, and the three Dependabot ecosystems are all still present. | bash only |

Run individually:

```bash
bash tools/supply-chain/test_deploy_gate.sh
bash tools/supply-chain/test_workflow_invariants.sh
```

## What is **not** covered here

These are environment-dependent and run in CI / against real images, not in
the local check:

- Real `cosign sign` + `cosign attest` against a real Sigstore Fulcio + Rekor
  — runs as part of the `cd-stg` workflow on every push to `main`.
- `syft <image>` schema validation (`cyclonedx-cli validate`,
  `spdx-tools verify`) — implicitly validated by Syft itself failing closed
  in the `cd-stg` SBOM steps.
- The "PR introducing a vulnerable Go module fails CI" test — runs against
  the live GitHub Dependabot alerts API and is exercised by the
  `security-alerts` workflow on every PR.
