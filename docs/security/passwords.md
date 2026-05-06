# Password storage and verification (SIN-62213)

This note records the parameters, the threat model, and the no-log policy
that govern `internal/iam/password.go`. ADR-0004 is the longer-form
follow-up; this file is the operational reference.

## Algorithm and parameters

We use **argon2id** via `golang.org/x/crypto/argon2`. The PHC encoding
stored in `users.password_hash` is:

```
$argon2id$v=19$m=65536,t=3,p=4$<base64(salt)>$<base64(hash)>
```

Constants are defined in `internal/iam/password.go`:

| Parameter      | Value      | Constant in source |
| -------------- | ---------- | ------------------ |
| Time cost      | 3          | `argonTime`        |
| Memory cost    | 64 MiB     | `argonMemoryKiB`   |
| Parallelism    | 4 lanes    | `argonThreads`     |
| Output length  | 32 bytes   | `argonKeyLen`      |
| Salt length    | 16 bytes   | `argonSaltLen`     |
| Version        | 0x13 (19)  | `argon2.Version`   |

These match the **second** RFC 9106 recommended profile (the first profile
costs 2 GiB of RAM per verify, which is incompatible with the staging
hardware budget). Revisit when production CPU/RAM headroom is measured.

## Why parameters are pinned, not honoured from the encoded string

`VerifyPassword` parses the PHC string defensively — it rejects malformed
input with `iam.ErrInvalidEncoding` before any derivation — but it
**ignores** the embedded `m`, `t`, `p` values when re-deriving. The
package constants are used instead. This is deliberate: a hostile DB row
that embeds `m=4096,t=1,p=1` would otherwise force verification to run at
that lower cost, which is exactly the downgrade attack we want to prevent.

A future migration that bumps the constants will need a re-hash path
(detect old-parameter rows on successful login, re-hash with the new
constants). That work is **out of scope** for SIN-62213 and is tracked
separately.

## Salt source

Every `HashPassword` call reads a fresh 16-byte salt from `crypto/rand`.
We never derive salt from the email, the user id, or any other deterministic
input. A salt-read failure aborts the hash with an error, never falling back
to a constant or to `math/rand`.

## Constant-time comparison

`VerifyPassword` compares the derived bytes to the stored bytes via
`crypto/subtle.ConstantTimeCompare`. Plain `bytes.Equal` (or `==` on the
strings) leaks length and prefix information through wall-clock timing.

## Anti-enumeration via dummy verify

`iam.Login` runs a full `VerifyPassword(password, dummyHash)` on the
not-found path. `dummyHash` is precomputed in `init()`. Without this,
"unknown email" returns in microseconds (DB miss + early return) while
"wrong password" takes ~100 ms (full argon2 verify); an on-the-wire
attacker can therefore enumerate which emails exist in a tenant.

The unit test `TestLogin_WrongPassword_NoEnumerate` asserts both paths
return `iam.ErrInvalidCredentials` AND that the not-found branch's
runtime is at least 25% of the wrong-password branch's runtime, with
median-of-3 sampling to keep the assertion CI-stable.

## No-log policy

The IAM package and its callers MUST NOT log:

- The plaintext password.
- The encoded `password_hash` string (full or prefix).
- The argon2 derived bytes.
- The user's email on a credential mismatch (logging it on a successful
  login is fine; logging it on a failure makes the access log a
  ready-made enumeration list).

What `iam.Login` *does* log:

- On success: `tenant_id`, `user_id`, `session_id_prefix` (first 8 hex
  chars of the v4 UUID — see `internal/iam/login.go` for the rationale).
- On rejection: `tenant_id`, `reason="invalid_credentials"` only.

## Production-only TOTP guard

`iam.NoopTOTP` accepts any non-empty code. It is for dev/test only.
`iam.AssertProductionSafe(verifier, getenv, exit)` aborts process startup
with `exit(1)` if `ENV=production` and the verifier is `NoopTOTP`. PR6
(`internal/adapter/httpapi`, [SIN-62217](/SIN/issues/SIN-62217)) is
responsible for calling this from `cmd/api/main.go` once the binary owns
DI wire-up.

## Cross-tenant isolation

`adapters/postgres.SessionStore.Get` and `LookupCredentials` both run
inside `WithTenant`. RLS hides cross-tenant rows; pgx's `ErrNoRows` is
the only signal an attacker sees. The error is translated to
`iam.ErrSessionNotFound` for sessions and to `(uuid.Nil, "", nil)` for
user-credential lookups, so cross-tenant probes are indistinguishable
from "id does not exist anywhere". Enforced by integration tests:

- `TestSessionStore_CrossTenantProbe_CollapsesToNotFound`
- `TestUserCredentialReader_CrossTenant_HiddenByRLS`

## Session id

`iam.NewSessionID` returns a fresh v4 UUID stamped explicitly from
`crypto/rand` (no dependence on the `uuid` package's internal entropy
source). 122 effective bits of randomness, well beyond brute-force reach.
The cookie payload is the canonical hyphenated UUID string.

## Session TTL

Default 24 h, overridable via `SESSION_TTL` (any value `time.ParseDuration`
accepts). At bootstrap, the value is sanity-bounded to the inclusive
range `[1m, 30d]`; out-of-range or unparseable values trigger a
`log.Fatal` so a misconfigured deploy does not silently issue
effectively-permanent or zero-TTL sessions. The "missing-unit foot-gun"
(`SESSION_TTL=24` parsed as 24 ns) is rejected by the lower bound.
