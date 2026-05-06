# Staging deploy runbook (Fase 0 PR9 / SIN-62215)

This page tells a new on-call engineer how to:

1. provision a staging VPS from scratch in under one hour,
2. read logs while a deploy is in flight,
3. roll back manually when the smoke check goes red,
4. understand which secrets the GitHub Actions CD job expects,
5. bump the SHA256 digest of an infra image (postgres, caddy, redis, nats, minio).

The CD pipeline itself is `.github/workflows/cd-stg.yml`; the staging stack is
`deploy/compose/compose.stg.yml`; the on-host wrapper invoked over SSH is
`deploy/scripts/stg-deploy.sh`.

## Pre-merge gates

Beyond `ci` / `paperclip-lint` / `aicache-lint` / `webhook-integration` (which
all install Go via `actions/setup-go` and never exercise the multi-stage
Dockerfile), one extra gate runs on every PR that touches the staging build
inputs:

| Gate                    | Workflow                                  | Triggers on path change of                                | What it catches                                                                |
| ----------------------- | ----------------------------------------- | --------------------------------------------------------- | ------------------------------------------------------------------------------ |
| `docker-smoke` (SIN-62301) | `.github/workflows/docker-smoke.yml`      | `Dockerfile`, `.dockerignore`, `go.mod`, `go.sum`          | Builder-image vs `go.mod` toolchain drift — fails fast at `go mod download` if the pinned base image cannot satisfy the source tree's `go`/`toolchain` directives. |
| `govulncheck` (SIN-62298) | `.github/workflows/govulncheck.yml`       | every PR                                                  | Reachable stdlib/dep CVEs (call-graph, source-mode).                          |

The gates are **complementary, not duplicates**: `govulncheck` runs against the
source tree's import graph; `docker-smoke` runs against the build sandbox the
staging image will actually compile in. Both fail closed.

`docker-smoke` builds the multi-stage `builder` target only — `push: false`,
`load: false`, GHA-scoped buildx cache. The failure mode that motivated the
gate (`[builder 4/7] RUN go mod download` against a builder image that does
not satisfy `toolchain go1.25.9`) surfaces before the runtime stage, so
building further would add wall-clock without adding signal. Cache-hit runs
land under ~1 min; cold builds land under the workflow's 8-min timeout.

**If you bump `go.mod` toolchain or `go` directive, also bump:**

- the builder `FROM` digest in `Dockerfile` (currently
  `golang:1.25.9-alpine@sha256:5caaf1cca…`) — see "Bumping infra image
  digests" below for the resolve-by-digest flow;
- `actions/setup-go` `go-version:` in `.github/workflows/ci.yml` (and the
  other Go-using workflows);
- the dev compose Go base in `deploy/compose/compose.yml` if it is in scope
  for your change.

`docker-smoke` will fail closed on the PR if the Dockerfile builder lags,
which is the postmortem fix-forward from SIN-62297 (the orphan-bridge merge
that was 8/8 green pre-merge and red 23s post-merge). The gate is also the
positive-control demonstration: regressing the builder image in a throwaway
PR turns it red.

## Architecture in one paragraph

Every push to `main` triggers `ci`. When `ci` finishes green, `cd-stg` wakes,
builds the multi-stage distroless image from `Dockerfile`, pushes it to GHCR,
and SSHes into the VPS. The VPS-side wrapper (`/opt/crm/stg/bin/deploy.sh`,
copied from `deploy/scripts/stg-deploy.sh`) rewrites only the `APP_IMAGE=`
line in `/opt/crm/stg/.env.stg`, runs `docker compose pull && up -d`, prunes
dangling images, and exits. The runner then `curl`s `https://${STG_SMOKE_URL}/health`
from outside; failure paints the job red. **No automatic rollback** — Fase 6
will revisit that.

Image policy: every image in `compose.stg.yml`, including the app, is consumed
by SHA256 digest, never by floating tag. `grep -E ':(latest|alpine)$'
deploy/compose/compose.stg.yml` MUST return zero matches; CI fails the build
if it ever does.

## Provisioning a fresh staging VPS (target: < 1h)

Assumes Debian 12 / Ubuntu 24.04 with a public IP and root SSH from a bastion.

Before starting, decide which tenant FQDNs the staging stack will host (Fase 0
defaults are `acme.crm.<base>` and `globex.crm.<base>`) and create A records
for each one pointing at the VPS public IP. Caddy uses Let's Encrypt's HTTP-01
challenge to issue certs and that requires the DNS already be in place; a
missing or wrong record means 443 silently never opens. Verify on a
workstation:

```bash
STG_HOST_IP="REPLACE_WITH_VPS_IP"
for host in acme.crm.REPLACE_WITH_BASE globex.crm.REPLACE_WITH_BASE; do
  got=$(dig +short "${host}" A | tail -n1)
  if [ "${got}" = "${STG_HOST_IP}" ]; then
    echo "ok ${host} → ${got}"
  else
    echo "MISMATCH ${host} → ${got:-empty}"
  fi
done
```

### 1. Base packages

Docker publishes separate apt repositories for Debian and Ubuntu — the URL
path is `linux/debian` on Debian and `linux/ubuntu` on Ubuntu. Set both
variables from `/etc/os-release` so the same script works on either distro
and fails fast if the host is something else.

```bash
apt-get update
apt-get install -y ca-certificates curl gnupg ufw fail2ban
install -m 0755 -d /etc/apt/keyrings
. /etc/os-release
case "$ID" in
  debian|ubuntu) DOCKER_DISTRO="$ID" ;;
  *)
    echo "unsupported distro: $ID — Docker repo only ships debian and ubuntu" >&2
    exit 1
    ;;
esac
curl -fsSL "https://download.docker.com/linux/${DOCKER_DISTRO}/gpg" | \
  gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
  https://download.docker.com/linux/${DOCKER_DISTRO} ${VERSION_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker
```

Quick troubleshooting if `apt-get install docker-ce` says
`Package docker-ce is not available`:

- `cat /etc/apt/sources.list.d/docker.list` — confirm the URL contains
  `linux/debian` on Debian or `linux/ubuntu` on Ubuntu (matching `$ID` above).
- `cat /etc/os-release | grep -E '^(ID|VERSION_CODENAME)='` — confirm the
  codename is one Docker actually ships (Debian: bullseye/bookworm/trixie,
  Ubuntu: focal/jammy/noble).
- Re-run `apt-get update` and watch the output for any `404 Not Found` lines
  pointing at `download.docker.com` — those mean the URL is wrong for the host.

### 2. Firewall and unattended upgrades

```bash
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw enable
apt-get install -y unattended-upgrades
dpkg-reconfigure -plow unattended-upgrades
```

### 3. Deploy user with constrained SSH

```bash
useradd --create-home --shell /bin/bash crm-deploy
usermod -aG docker crm-deploy
install -d -o crm-deploy -g crm-deploy -m 0700 /home/crm-deploy/.ssh
```

Generate the CD SSH keypair on a workstation (NOT on the VPS), keep the
private half off the host, and install the public half with a strict
`command=` constraint so the key cannot do anything except invoke the
deploy script:

```bash
# on a workstation (or vault host)
ssh-keygen -t ed25519 -C "github-actions cd-stg" -f cd-stg-ed25519 -N ""

# Print the entire pubkey on a single line — copy this whole line to the
# clipboard. It already starts with `ssh-ed25519 AAAA…` and ends with the
# comment `github-actions cd-stg`; do NOT add `ssh-ed25519 ` again on the VPS.
cat cd-stg-ed25519.pub
```

Then on the VPS, paste the entire pubkey into a single shell variable and
let the heredoc interpolate it. Using a variable instead of a literal
placeholder prevents the common footgun of half-replacing the placeholder
and ending up with `ssh-ed25519 AAAA…ssh-ed25519 AAAA<real-key>`, which sshd
silently rejects.

```bash
# Replace REPLACE_… with the EXACT contents of cd-stg-ed25519.pub from the
# workstation — one line, starts with `ssh-ed25519 `, ends with the comment.
PUBKEY="REPLACE_WITH_ENTIRE_LINE_FROM_cd-stg-ed25519.pub"
cat > /home/crm-deploy/.ssh/authorized_keys <<AUTH
command="/opt/crm/stg/bin/deploy.sh",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc ${PUBKEY}
AUTH
chown crm-deploy:crm-deploy /home/crm-deploy/.ssh/authorized_keys
chmod 600 /home/crm-deploy/.ssh/authorized_keys

# Sanity: file should have exactly one line, no `AAAA…` literal, and exactly
# one occurrence of `ssh-ed25519 `:
test "$(grep -c 'ssh-ed25519 ' /home/crm-deploy/.ssh/authorized_keys)" = "1" \
  || { echo "authorized_keys malformed: pub key duplicated or placeholder kept"; exit 1; }
```

The private half goes into the GitHub repo's `STG_SSH_KEY` secret. Once the
constraint is in place the key cannot start a shell, open a tunnel, or run
arbitrary commands — only `/opt/crm/stg/bin/deploy.sh` with a single argument.

### 4. Stack layout on the VPS

This step lays down `/opt/crm/stg/` on the VPS itself. Run it BEFORE the first
deploy in §5 — `compose.stg.yml` and the deploy wrapper need to be present
before `/opt/crm/stg/bin/deploy.sh` can be invoked.

The repo is private, so `raw.githubusercontent.com` cannot serve the two
artifacts anonymously. Push them from a workstation that already has the repo
cloned (the same workstation you used in §3 to generate the CD SSH keypair):

```bash
# On the workstation, in the cloned `crm` repo root.
# Replace REPLACE_STG_HOST with the staging VPS hostname or IP.
STG_HOST="REPLACE_STG_HOST"
scp deploy/compose/compose.stg.yml deploy/scripts/stg-deploy.sh \
    "root@${STG_HOST}:/tmp/"
# Caddy reads its config from /etc/caddy/, mounted from /opt/crm/stg/caddy/.
# Send the two files Caddy needs at startup:
scp deploy/caddy/Caddyfile.stg deploy/caddy/security-headers.caddy \
    "root@${STG_HOST}:/tmp/"
```

Back on the VPS, lay out the stack directory and install all four files. The
operator running this block must be `root` (or in a sudo session) — the
`crm-deploy` account exists but has no shell.

```bash
# Sanity check: confirm scp landed everything in /tmp.
for f in compose.stg.yml stg-deploy.sh Caddyfile.stg security-headers.caddy; do
  test -s "/tmp/${f}" || { echo "missing /tmp/${f}"; exit 1; }
done

# Lay out the stack directory and install the four files into it.
install -d -o crm-deploy -g crm-deploy -m 0750 \
  /opt/crm/stg /opt/crm/stg/bin /opt/crm/stg/caddy
install -o crm-deploy -g crm-deploy -m 0640 \
  /tmp/compose.stg.yml /opt/crm/stg/compose.stg.yml
install -o root -g crm-deploy -m 0750 \
  /tmp/stg-deploy.sh /opt/crm/stg/bin/deploy.sh
install -o crm-deploy -g crm-deploy -m 0640 \
  /tmp/Caddyfile.stg /opt/crm/stg/caddy/Caddyfile.stg
install -o crm-deploy -g crm-deploy -m 0640 \
  /tmp/security-headers.caddy /opt/crm/stg/caddy/security-headers.caddy

# Empty secrets file with the right ownership; you fill it in below.
install -o crm-deploy -g crm-deploy -m 0640 /dev/null /opt/crm/stg/.env.stg
```

If you ever bump `compose.stg.yml`, `stg-deploy.sh`, or any file under
`deploy/caddy/` on `main`, repeat the same `scp` + `install` flow from a
workstation — the CD pipeline only pushes the application image, not these
on-host artifacts. Automating that sync is tracked as a follow-up; until
then it is operator-driven.

Generate the two infra passwords. They land in `DATABASE_URL` and the MinIO
admin credential, so they MUST be alphanumeric (no `@`, `:`, `/`, `?` —
those break URL parsing in `postgres://user:pass@host/...`). 256 bits of hex
is a safe, copy-paste-friendly default:

```bash
openssl rand -hex 32   # POSTGRES_PASSWORD
openssl rand -hex 32   # MINIO_ROOT_PASSWORD
```

Run each line once, store the outputs in your password manager (1Password,
Bitwarden, etc.) before pasting into `.env.stg`. Losing them later means
recreating the volumes from scratch.

Fill `/opt/crm/stg/.env.stg`. Anything in `REPLACE_…` is a placeholder you
must overwrite — do NOT keep the angle-bracket-style `<digest>` form, bash
parses `<` as input redirection and the line will fail with
`syntax error near unexpected token 'newline'`.

```dotenv
POSTGRES_DB=crm
POSTGRES_USER=crm
POSTGRES_PASSWORD=REPLACE_WITH_HEX_FROM_OPENSSL_RAND
MINIO_ROOT_USER=crm-admin
MINIO_ROOT_PASSWORD=REPLACE_WITH_HEX_FROM_OPENSSL_RAND
HSTS_MAX_AGE=300
# Let's Encrypt account contact for cert issuance / expiry warnings. MUST be
# a real RFC 5322 address with a valid TLD — Let's Encrypt and ZeroSSL both
# reject anything else with HTTP 400 invalidContact ("Domain name contains
# an invalid character") and Caddy retries forever with no certs ever issued.
# `name@example.com` is fine; `name@REPLACE_…` is not.
ACME_EMAIL=REPLACE_WITH_REAL_OPS_EMAIL
# Comma-separated list of tenant FQDNs Caddy provisions certs for. Order
# does not matter; every entry must already have an A record pointing at
# the VPS public IP (verify with the dig loop above).
STG_TENANT_HOSTS=acme.crm.REPLACE_WITH_BASE, globex.crm.REPLACE_WITH_BASE
# APP_IMAGE is rewritten by the deploy wrapper on every push. Bootstrap with
# the digest you discover in §5 below — full ref like
# ghcr.io/pericles-luz/crm@sha256:6b8f…f730ba.
APP_IMAGE=REPLACE_WITH_INITIAL_DIGEST_REF
```

### 4b. GHCR pull credentials for the deploy user

GHCR inherits visibility from the source repository, so `crm` images are
private by default. The `crm-deploy` user must be authenticated against
`ghcr.io` BEFORE the first deploy or `docker compose pull` returns
`unauthorized`. There are two acceptable paths — pick one and stick with it.

#### Path A — classic PAT with `read:packages` on the VPS (non-public stg)

GitHub fine-grained PATs do NOT support package access for **user-owned**
packages — the `Packages` permission only appears for organization-owned
ones. For `ghcr.io/pericles-luz/crm`, use a classic PAT with the single
`read:packages` scope (which is itself the smallest scope GitHub exposes for
this use case).

1. On the workstation, open
   `https://github.com/settings/tokens/new?description=ghcr-stg-pull&scopes=read:packages`.
   That URL pre-selects the only required scope; do NOT enable any other
   checkbox. Set the expiry to whatever your rotation policy allows (90 days
   is a sensible default).
2. Copy the token (it starts with `ghp_…`) and run on the VPS:
   ```bash
   GHCR_USER="pericles-luz"
   GHCR_TOKEN="REPLACE_WITH_CLASSIC_PAT"
   sudo -u crm-deploy bash -c "echo '${GHCR_TOKEN}' | docker login ghcr.io -u '${GHCR_USER}' --password-stdin"
   ```
   That writes `~crm-deploy/.docker/config.json` with the encoded
   credential. Subsequent `docker compose pull` runs as `crm-deploy` reuse
   the same file silently.
3. Rotation: generate a new PAT and re-run the `docker login` line — old
   tokens are superseded in `config.json` automatically. Revoke the old PAT
   in the GitHub UI once the new one is in place.

If `crm` ever moves under an organization, switch to a fine-grained PAT
scoped to that org with `Packages: Read-only` and `crm` selected — that
form does work for org packages.

#### Path B — make the GHCR package public

If staging-image visibility is acceptable (no embedded secrets, no
proprietary code beyond what is already inferred from the public
distroless+Go binary): visit
`https://github.com/users/pericles-luz/packages/container/crm/settings`,
scroll to `Danger zone → Change visibility`, switch to `Public`. After that
no `docker login` is needed on the VPS — anonymous pulls succeed.

The CD workflow does not care which path you picked; it pushes with
`secrets.GITHUB_TOKEN` either way.

### 5. First boot

The CD pipeline only takes over once the VPS already runs at least one
working deploy. Until that bootstrap deploy lands, find the digest of an
image already pushed to GHCR and feed it into `deploy.sh` manually.

#### 5a. Find an image digest in GHCR

The `cd-stg` workflow has built and pushed images for every push to `main`
since SIN-62215 merged, even when the SSH step failed (build/push happens
before SSH). Pick whichever digest you want online first:

- **GitHub UI** — open `https://github.com/users/pericles-luz/packages/container/package/crm`,
  click into the version row that matches the SHA you want, and copy the
  `sha256:…` digest from the page header.
- **`gh` CLI on a workstation** — `gh run view <RUN_ID> --repo pericles-luz/crm --log`
  on a recent `cd-stg` run, then grep the `build & push image` block for
  `pushing manifest for ghcr.io/pericles-luz/crm:…@sha256:…` — the digest
  immediately after `@` is what you want.

#### 5b. Run the first deploy

Pin the discovered digest to a shell variable to avoid re-typing it (and to
sidestep the `<digest>` placeholder trap):

```bash
DIGEST="sha256:REPLACE_WITH_64_HEX_DIGEST"
APP_IMAGE_REF="ghcr.io/pericles-luz/crm@${DIGEST}"

# 1. Make .env.stg agree with what we are about to deploy.
sudo sed -i "s|^APP_IMAGE=.*|APP_IMAGE=${APP_IMAGE_REF}|" /opt/crm/stg/.env.stg
sudo grep '^APP_IMAGE=' /opt/crm/stg/.env.stg   # sanity

# 2. Run the deploy wrapper as the constrained user (NOT root):
sudo -u crm-deploy /opt/crm/stg/bin/deploy.sh deploy "${APP_IMAGE_REF}"

# 3. Internal smoke check first — confirms the app/caddy/network plumbing
#    works without depending on Let's Encrypt:
sudo -u crm-deploy docker exec crm-stg-caddy-1 wget -qO- http://app:8080/health

# 4. External smoke check. The first hit on each tenant FQDN triggers
#    Let's Encrypt issuance, which usually takes 5–30s. If the first curl
#    returns 525/timeout, retry once after 30 s. (Replace the URL with the
#    same value you will set in the STG_SMOKE_URL secret.)
STG_SMOKE_URL="https://acme.crm.REPLACE_WITH_STG_DOMAIN"
curl -fsS "${STG_SMOKE_URL}/health"
```

If both smoke checks return `{"status":"ok"}` you are done; subsequent
deploys are fully automated by the `cd-stg` workflow once you finish §6 and
populate the GitHub Actions secrets.

If the external check times out on `port 443` indefinitely, in that order:

1. `sudo -u crm-deploy docker logs crm-stg-caddy-1 --tail 50` — Caddy logs
   the Let's Encrypt failure inline; common offenders are:
   - **`invalidContact: contact email has invalid domain`** — `ACME_EMAIL`
     in `.env.stg` still has a placeholder or otherwise invalid TLD (`_`,
     `REPLACE_…`, etc). Fix the value, then `compose up -d --force-recreate
     caddy` (env vars only re-read on container creation, not on `restart`).
   - Missing or wrong DNS A records.
   - UFW blocking 80 (LE needs **both** 80 and 443 — 80 for the HTTP-01
     challenge, 443 for the eventual cert handshake).
2. `dig +short <fqdn>` from a workstation — confirm DNS resolves to the VPS.
3. `sudo ufw status` on the VPS — confirm 80 and 443 are allowed.

### 6. Capturing the staging host key

The runner verifies the VPS host key via `STG_HOST_KEY` to avoid TOFU. Capture
it once during provisioning:

```bash
STG_HOST="REPLACE_STG_HOST"
ssh-keyscan -t ed25519 "${STG_HOST}" | tee stg.host_key
```

Paste the output of that file into the `STG_HOST_KEY` GitHub Actions secret.

## Reading logs

`docker compose` parses `compose.stg.yml` even for read-only operations like
`logs`, and `compose.stg.yml` uses `${VAR:?…}` placeholders, so every
invocation needs `--env-file /opt/crm/stg/.env.stg` — otherwise compose errors
with `required variable … is missing a value` before it ever reads container
state.

```bash
COMPOSE_ARGS="--env-file /opt/crm/stg/.env.stg -f /opt/crm/stg/compose.stg.yml"

# All services, follow:
sudo -u crm-deploy docker compose ${COMPOSE_ARGS} logs -f --tail=200
# Single service:
sudo -u crm-deploy docker compose ${COMPOSE_ARGS} logs -f --tail=500 app
# What is currently deployed:
sudo -u crm-deploy grep '^APP_IMAGE=' /opt/crm/stg/.env.stg
sudo -u crm-deploy cat /opt/crm/stg/.last-image  # what was running before this deploy
```

## Manual rollback (smoke check went red)

The deploy wrapper records the previous `APP_IMAGE` in `/opt/crm/stg/.last-image`
just before swapping. To revert:

```bash
STG_SMOKE_URL="https://acme.crm.REPLACE_WITH_STG_DOMAIN"
prev="$(cat /opt/crm/stg/.last-image)"
sudo -u crm-deploy /opt/crm/stg/bin/deploy.sh deploy "${prev}"
curl -fsS "${STG_SMOKE_URL}/health"
```

If the rollback also fails, `compose down && compose up` is the next escalation;
beyond that, file a ticket and page the on-call.

## GitHub Actions secrets used by `cd-stg`

| Secret             | Purpose                                                                          |
| ------------------ | -------------------------------------------------------------------------------- |
| `GITHUB_TOKEN`     | Auto-issued by Actions; `packages: write` is enough to push to `ghcr.io`.        |
| `STG_SSH_KEY`      | Private half of the CD ed25519 keypair (locked by `command=` on the VPS).        |
| `STG_HOST`         | DNS name or IP of the staging VPS.                                               |
| `STG_HOST_KEY`     | Output of `ssh-keyscan -t ed25519 <stg-host>` — populates the runner's known_hosts. |
| `STG_USER`         | Unprivileged deploy user (`crm-deploy` in this runbook).                         |
| `STG_SMOKE_URL`    | Base URL the runner curls — e.g. `https://acme.crm.<stg-domain>`.                |

`GHCR_TOKEN` is not used; the auto-issued `GITHUB_TOKEN` is preferred because
it rotates on every run and is scoped to this repo's packages only.

## Bumping infra image digests

`compose.stg.yml` pins postgres, caddy, redis, nats, and minio by SHA256 digest.
When you want to take a security or feature update for one of them:

1. Pick the new versioned tag (e.g. `postgres:16.5-alpine3.20`). Floating tags
   such as `:latest`, `:16-alpine`, `:7-alpine` are forbidden — the
   `grep -E ':(latest|alpine)$' deploy/compose/compose.stg.yml` check will
   reject them in CI.
2. Resolve the digest. Either tool below works, no docker daemon required:
   ```bash
   # docker buildx (when local docker is available):
   docker buildx imagetools inspect postgres:16.5-alpine3.20 \
     --format '{{ .Manifest.Digest }}'
   # or via curl + the registry HTTP API:
   tok=$(curl -fsS \
     "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/postgres:pull" \
     | jq -r .token)
   curl -fsSI \
     -H "Authorization: Bearer $tok" \
     -H "Accept: application/vnd.oci.image.index.v1+json" \
     -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
     https://registry-1.docker.io/v2/library/postgres/manifests/16.5-alpine3.20 \
     | awk '/^[Dd]ocker-[Cc]ontent-[Dd]igest:/ { print $2 }'
   ```
3. Replace the line in `compose.stg.yml` with the new `tag@sha256:…` reference,
   keeping the human-readable tag for context.
4. Run the AC #6 check locally:
   ```bash
   grep -nE ':(latest|alpine)$' deploy/compose/compose.stg.yml && exit 1 || true
   ```
5. Open a normal PR. Reviewers see the new digest, the CI gate enforces the
   shape, and the next push to `main` deploys it via `cd-stg`.

## Known limitations (Fase 0 scope)

- **No automatic rollback.** AC #3 of SIN-62215 deliberately keeps rollback
  manual; auto-rollback is parked for Fase 6 or a follow-up issue.
- **Single-tenant smoke check.** The runner curls one URL (`STG_SMOKE_URL`).
  The "two tenants in stg" goal of Fase 0 critério #2 is satisfied by the
  Caddy wildcard host plus a future per-tenant probe (Fase 1 follow-up).
- **No host-level Prometheus / log shipping yet.** PR10/11 of Fase 0 wires
  alerting and observability; the runbook above is what we have until then.
