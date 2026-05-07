# ADR 0070 — Password hashing (Argon2id) e política de senha

- Status: Accepted
- Date: 2026-05-07
- Deciders: CTO, SecurityEngineer, Coder (impl)
- Issue: [SIN-62336](/SIN/issues/SIN-62336) — F12 (CRITICAL) + F18 of the
  Fase 0 security review ([SIN-62220](/SIN/issues/SIN-62220#document-security-review))
- Parent: [SIN-62223](/SIN/issues/SIN-62223) — Password hashing + CSRF + master 2FA em Fase 0
- Plan: [SIN-62223 doc decisions §1 + §4](/SIN/issues/SIN-62223#document-decisions)

## Context

Fase 0 of the CRM brings up first-party authentication. The migration in
[SIN-62192](/SIN/issues/SIN-62192) (PR 2) already created the
`user.password_hash text not null` column; what is missing is the *contract*
that decides what bytes go in there. Without that contract pinned, the first
piece of auth code lands with an unsafe default and every subsequent commit
inherits the choice — F12 in the security review is rated CRITICAL for
exactly this reason.

Two decisions need to be fixed before any code in `internal/iam/password`
gets written:

1. **F12 — what hashing algorithm and parameters do we store?** The
   answer drives migration shape, login latency budget, and the
   crypto-agility story (re-hash on parameter change).
2. **F18 — what password policy do we enforce at the boundary?** Strong
   hashing of a `123456` is still a `123456`. NIST 800-63B has rewritten
   the playbook here (no composition rules, no mandatory expiry, screen
   against breach corpora) and we want to land on that side rather than
   re-litigate it later.

This ADR fixes both. Implementation lives in
[SIN-62223](/SIN/issues/SIN-62223) and its sibling implementation issue.

## Decision

### §1 — Algorithm: Argon2id via `golang.org/x/crypto/argon2`

We hash passwords with **Argon2id**, the OWASP-recommended password KDF
since 2021 and the winner of the Password Hashing Competition. `argon2id`
is the hybrid mode: data-independent first pass (resists side-channel /
timing attacks on a known-plaintext attacker) followed by data-dependent
passes (resists time-memory trade-offs on a database-leak attacker).

The Go implementation is `golang.org/x/crypto/argon2`, which is the
upstream maintained by the Go team and already vendored transitively in
the build. No new direct dependency is added beyond pinning that module.

Initial parameters, calibrated on the staging runner (which mirrors the
production VPS class):

| Parameter         | Value     | Rationale |
|-------------------|-----------|-----------|
| `memory` (`m`)    | 64 MiB    | OWASP 2024 minimum for Argon2id; pushes GPU/ASIC attackers off cheap silicon |
| `iterations` (`t`)| 3         | OWASP 2024 minimum at `m=64MB`; lands inside the latency budget |
| `parallelism` (`p`)| 1        | Per-process thread; the request handler is the parallelism unit, not the hash |
| `saltLen`         | 16 bytes  | Random per-hash, from `crypto/rand` |
| `keyLen`          | 32 bytes  | Argon2id digest size |

Wall-clock target on prod hardware: **~250 ms per `Hash` call**. The
benchmark (see §6) asserts the band `150 ms ≤ t ≤ 400 ms` so a runner
upgrade or downgrade fails CI loudly instead of silently weakening or
overshooting the budget.

### §2 — Stored format

The encoded hash is a single `text` value with five `$`-separated fields,
matching the de-facto Argon2 PHC string format:

```
argon2id$v=19$m=65536,t=3,p=1$<salt-b64>$<hash-b64>
```

- `argon2id` — algorithm identifier (lowercase, exact).
- `v=19` — Argon2 version 0x13, which is what `x/crypto/argon2` emits.
- `m=65536,t=3,p=1` — parameters in `kibibytes,iterations,parallelism`
  order. 65536 KiB == 64 MiB, matching §1.
- `<salt-b64>` — RawStdEncoding (no padding) of 16 random bytes.
- `<hash-b64>` — RawStdEncoding (no padding) of the 32-byte digest.

This is the same shape the OpenWall reference and most language-level
Argon2 wrappers emit, so externally-imported hashes (e.g., from a future
SSO migration) can be parsed by the same `Verify` path. The encoded
string lives in `user.password_hash`; no new column is needed.

`Verify` MUST parse the parameters out of the stored string — never
trust a global "current params" constant — so old rows decode correctly
during a parameter-bump rollout.

### §3 — Crypto agility: re-hash on login

Whenever a successful login decodes a stored hash whose `(m, t, p)`
differs from the current values in §1, the verifier returns
`needsRehash = true` and the calling auth use-case re-hashes the
plaintext (still in memory) with current parameters and writes the new
encoded value to `user.password_hash` in a **separate transaction** from
the login itself.

Why a separate transaction:

- The login response is on the latency-critical path. A rehash failure
  (DB hiccup, lock contention) MUST NOT fail the login — the existing
  hash is already known-valid.
- The rehash write is logged at WARN level on failure with the user id,
  the stored-vs-target params, and the underlying error. It is retried
  opportunistically on the next successful login.

This gives us a clean upgrade path: bump §1, deploy, and over the next
N days every active account silently lifts to the new params without a
batch migration. Inactive accounts stay on the old params until next
login or until a future explicit batch (out of scope for this ADR).

### §4 — Optional pepper (defense in depth, deferred)

A *pepper* is a server-side secret mixed into the hash input that is
**not** stored in the database. If `user` is dumped without the
application secrets, every hash in the dump becomes uncrackable —
attackers cannot even confirm a guess against a leaked row.

We do not require the pepper for Fase 0 — Argon2id at the parameters in
§1 already passes the security review's must-have bar — but the design
keeps the door open:

- The pepper, when present, is read from env var `IAM_PASSWORD_PEPPER`
  (rotatable, never logged, never in the connection string).
- `Hash` and `Verify` both accept it via the `PolicyContext` (§6) so
  the helper signature does not change when it is adopted.
- Adoption requires **no schema migration**: the pepper participates in
  `argon2.IDKey(plain || pepper, salt, …)` at compute time. The stored
  PHC string is unchanged.
- Rotation is handled by carrying both `IAM_PASSWORD_PEPPER` (current)
  and `IAM_PASSWORD_PEPPER_PREV` (previous) for a deprecation window;
  `Verify` tries each in turn, marks `needsRehash = true` when the
  match came from `_PREV`, and §3 carries the row forward to the
  current pepper.

The pepper rollout itself is its own ADR/ticket when we adopt it.

### §5 — Password policy (F18, NIST 800-63B-aligned)

The policy is enforced at the request boundary by
`password.PolicyCheck(plain, ctx)`. Domain code never inspects
plaintext — only the boundary handler and the helper see it.

| Rule | Value | Rationale |
|------|-------|-----------|
| Minimum length | 12 chars | NIST 800-63B 5.1.1.2; OWASP ASVS V2.1.1; matches breach-cost economics today |
| Maximum length | 128 chars | Truncation guard; prevents DoS by megabyte-passwords through Argon2 |
| Composition rules (mixed case, digits, symbols) | **None** | NIST 800-63B explicitly removes them — they reduce entropy in practice (predictable substitutions) |
| Breach-corpus screening | HaveIBeenPwned k-anonymity | NIST 800-63B 5.1.1.2 "verifiers SHALL compare against a list of values known to be commonly used or compromised" |
| Local fallback list | Top-100k breached passwords (compiled in) | Offline gate so HIBP outage cannot let weak passwords in (see fail-closed below) |
| Identity check | Password ≠ email, username, or tenant name (case-insensitive, normalized) | Closes the easiest credential-stuffing seed |
| Mandatory expiry | **None** | NIST 800-63B 5.1.1.2 — forced rotation is net-negative; rotate on suspected compromise instead |
| Rate limiting | Handler-side (separate ADR) | Policy is a pure function; throttling is a transport concern |

**HIBP adapter — fail-closed shape.** The HIBP check is a port
(`PwnedPasswordChecker`) with one production adapter
(`adapter/iam/hibp`) that calls
`https://api.pwnedpasswords.com/range/<sha1-prefix-5>` and matches the
remaining 35 hex chars locally. The adapter wraps a circuit breaker:

- Closed: query the remote, treat any non-zero count as breached.
- Open (after N consecutive errors): short-circuit, **do not** treat
  the password as clean — fall through to the local top-100k list.
  If the local list rejects the password, the policy fails. If the
  local list passes the password, the policy passes **with a warning
  audit log entry** (`event=iam_password_hibp_unavailable`) so we can
  see how often we are operating without the remote gate.
- Half-open: probe periodically; close on success.

This is "fail open with guardrails": HIBP being down does not let a
known-top-100k password through, but it does let an *unknown* weak
password through that HIBP would have caught. The trade-off is
deliberate — making the remote check a hard gate would let any HIBP
outage halt all signups/password-changes, which is a worse availability
posture than the residual risk of a not-top-100k weak password slipping
through during an outage.

### §6 — Helper API contract

The helper lives at `internal/iam/password` as a pure-domain package.
It does not import `database/sql`, `net/http`, or vendor SDKs — every
external dependency (HIBP, audit log, clock) is a port.

```go
package password

// Hash hashes plain with the current §1 parameters and returns the
// encoded PHC string (§2). It does not consult the policy — callers
// MUST run PolicyCheck first; Hash assumes plain has already passed.
func Hash(plain string) (string, error)

// Verify decodes stored, runs Argon2id with the parsed parameters and
// returns:
//   - ok:          the supplied plain matches the stored hash
//   - needsRehash: stored params differ from current §1 (re-hash on
//                  login per §3), or the match came from the previous
//                  pepper (§4)
//   - err:         decoding or computation failure (NEVER mismatch —
//                  a mismatch is ok=false, err=nil)
func Verify(stored, plain string) (ok bool, needsRehash bool, err error)

// PolicyCheck validates plain against §5. ctx carries the per-request
// values needed for the identity check (email, username, tenant name)
// and the optional pepper for §4. It returns nil on pass, a typed
// PolicyError on the first failing rule (so the caller can render the
// localized message it wants).
func PolicyCheck(plain string, ctx PolicyContext) error

type PolicyContext struct {
    Email      string
    Username   string
    TenantName string
    Pepper     string // optional, see §4
}
```

`Hash` and `Verify` are deterministic-modulo-the-salt — they take the
clock from `time.Now()` only for audit-log breadcrumbs, never from the
hash input.

### §7 — Verification (tests + benchmark)

Three pieces of verification land alongside the implementation in
`internal/iam/password`:

1. **Vetores fixos** — table-driven tests with hand-checked PHC strings
   (small `m`, small `t` for speed) prove that the `Hash` / `Verify`
   round-trips match the format in §2 byte-for-byte and that `Verify`
   accepts hashes produced by reference implementations
   (e.g. `argon2-cli`).
2. **Property tests** — random-plaintext-and-salt round-trips assert
   `Hash → Verify(ok=true, needsRehash=false)` and that any one-bit
   flip in the encoded string flips `ok` to false without erroring.
3. **Benchmark gate** — `BenchmarkHashProductionParams` hashes with the
   §1 production parameters and **fails the test** if median wall-clock
   is outside `150 ms ≤ t ≤ 400 ms`. Runs on the standard CI runner,
   so a runner change that doubles speed (cheap GPU-class instance)
   forces us to re-tune; a runner change that halves speed (slower
   ARM tier) forces us to look at the budget before login UX silently
   degrades.

If the benchmark band is breached, the response is to **re-tune §1 in a
follow-up ADR amendment**, not to relax the assertion. The amendment
records the new parameters, the new measurement, and the runner class
that produced it.

## Lenses applied

- **Hexagonal / Ports & Adapters.** `internal/iam/password` is pure
  domain. HIBP is a port + adapter pair; the audit log and clock are
  ports too. Nothing in `internal/iam/password` imports `net/http` or
  `database/sql` and the package's tests do not need a database.
- **Defense in depth.** Argon2id assumes a database breach; the §4
  pepper assumes the breach plus filesystem access; the §5 policy
  assumes the user picks a weak password. Each layer has to fail
  before the next is reached.
- **Secure-by-default API.** Calling `Hash` with an empty string or
  `Verify` with a malformed PHC string returns a typed error — there
  is no silent path that produces a "valid" empty digest. `PolicyCheck`
  has no "skip" flag.
- **Boring technology budget.** Argon2id ships in `golang.org/x/crypto`
  (Go-team-maintained, already in the build graph). The HIBP integration
  is a stdlib `net/http` client behind a port. No new heavy dependency,
  no third-party password library, no microservice.

## Consequences

Positive:

- F12 is closed: every login from Fase 0 onward stores Argon2id at the
  current parameters; `needsRehash` lifts old rows quietly as users
  log in.
- F18 is closed: weak passwords are rejected at the boundary, NIST
  guidance is followed, breach-corpus screening is in place with a
  documented availability trade-off.
- Crypto agility is built in: bumping §1 is a parameter change, a
  redeploy, and time. No batch migration script.
- Pepper adoption (§4) does not require a migration when we are ready
  for it.

Negative / costs:

- Login latency picks up ~250 ms server-side. This is the explicit
  budget — we eat it because the alternative is a hash an attacker can
  brute-force on a desktop.
- Memory pressure: Argon2id at `m=64MB` allocates 64 MiB of scratch
  per concurrent login. At our request concurrency this is comfortably
  within the VPS memory budget, but a future autoscaling story has to
  account for it.
- The HIBP fail-open posture means a sustained outage of
  `api.pwnedpasswords.com` lets *some* weak passwords through. The
  audit log surfaces the rate; if it ever spikes, the response is to
  expand the local list, not to flip the gate to fail-closed.

## Alternatives considered

- **bcrypt.** OWASP still lists it as acceptable, but its 72-byte
  truncation, lack of memory hardness, and plateauing cost factor make
  Argon2id the clear forward choice for greenfield code. Rejected.
- **scrypt.** Memory-hard but tuning is awkward (single `N` knob
  conflates time and memory) and the PHC adoption story is weaker than
  Argon2id's. Rejected.
- **Argon2i (data-independent only).** Misses the GPU-resistance
  benefit of the data-dependent passes. Rejected by the PHC final
  recommendation in favor of Argon2id.
- **Pepper as a hard requirement in Fase 0.** Adds a secret-management
  story (rotation, KMS, env-injection across staging + prod) before
  the auth flow even ships. Deferred per §4 — adoption later is a
  no-migration code change.
- **Composition rules (uppercase, digit, symbol).** NIST 800-63B
  removed them in the 2017 revision; ASVS followed. Rejected — they
  reduce entropy in practice and the 12-char minimum + breach-corpus
  screening dominates them.
- **Mandatory expiry.** Same rationale, rejected. Forced rotation
  trains users to predictable patterns. We rotate on suspected
  compromise instead.

## Out of scope

- Implementation of `internal/iam/password` and the HIBP adapter —
  child issue under [SIN-62223](/SIN/issues/SIN-62223).
- The login / signup / change-password handlers and their CSRF +
  rate-limit story — siblings of this ADR under
  [SIN-62223](/SIN/issues/SIN-62223).
- Password reset flow (email-token-based) — separate ticket.
- Master 2FA — separate decision under [SIN-62223](/SIN/issues/SIN-62223).
- KMS-backed pepper rotation — gated on §4 adoption.

## Rollback

This is a doc-only ADR; rollback is editing the file. The
implementation that follows can be reverted by:

- Reverting the `internal/iam/password` package and the HIBP adapter.
- The `user.password_hash` column stays in place — it is plain `text`
  and can hold any future encoding. No data migration is required to
  adopt a different hash format because §3's `Verify` already parses
  the algorithm prefix; a future ADR can add a second prefix and
  let `needsRehash` carry rows forward.
