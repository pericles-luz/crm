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

# copy cd-stg-ed25519.pub to the VPS, then on the VPS:
cat <<'AUTH' > /home/crm-deploy/.ssh/authorized_keys
command="/opt/crm/stg/bin/deploy.sh",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc ssh-ed25519 AAAA…<paste cd-stg-ed25519.pub here>
AUTH
chown crm-deploy:crm-deploy /home/crm-deploy/.ssh/authorized_keys
chmod 600 /home/crm-deploy/.ssh/authorized_keys
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
# On the workstation, in the cloned `crm` repo root:
scp deploy/compose/compose.stg.yml deploy/scripts/stg-deploy.sh \
    root@<stg-host>:/tmp/
```

Back on the VPS, lay out the stack directory and install both files. The
operator running this block must be `root` (or in a sudo session) — the
`crm-deploy` account exists but has no shell.

```bash
# Sanity check: confirm scp landed both files in /tmp.
test -s /tmp/compose.stg.yml && test -s /tmp/stg-deploy.sh

# Lay out the stack directory and install the two files into it.
install -d -o crm-deploy -g crm-deploy -m 0750 /opt/crm/stg /opt/crm/stg/bin
install -o crm-deploy -g crm-deploy -m 0640 \
  /tmp/compose.stg.yml /opt/crm/stg/compose.stg.yml
install -o root -g crm-deploy -m 0750 \
  /tmp/stg-deploy.sh /opt/crm/stg/bin/deploy.sh

# Empty secrets file with the right ownership; you fill it in below.
install -o crm-deploy -g crm-deploy -m 0640 /dev/null /opt/crm/stg/.env.stg
```

If you ever bump `compose.stg.yml` or `stg-deploy.sh` on `main`, repeat the
same `scp` + `install` flow from a workstation — the CD pipeline only pushes
the application image, not these on-host artifacts. Automating that sync is
tracked as a follow-up; until then it is operator-driven.

Fill `/opt/crm/stg/.env.stg` with:

```dotenv
POSTGRES_DB=crm
POSTGRES_USER=crm
POSTGRES_PASSWORD=<from vault>
MINIO_ROOT_USER=crm-admin
MINIO_ROOT_PASSWORD=<from vault>
HSTS_MAX_AGE=300
# APP_IMAGE is rewritten by the deploy wrapper on every push; bootstrap with the
# initial image you want online, e.g. the most recent main:
APP_IMAGE=ghcr.io/pericles-luz/crm@sha256:<digest>
```

### 5. First boot

```bash
sudo -u crm-deploy /opt/crm/stg/bin/deploy.sh deploy ghcr.io/pericles-luz/crm@sha256:<digest>
curl -fsS https://acme.crm.<stg-domain>/health
```

If `/health` returns `{"status":"ok"}` you are done; subsequent deploys are
fully automated by the `cd-stg` workflow.

### 6. Capturing the staging host key

The runner verifies the VPS host key via `STG_HOST_KEY` to avoid TOFU. Capture
it once during provisioning:

```bash
ssh-keyscan -t ed25519 <stg-host> | tee stg.host_key
```

Paste the output of that file into the `STG_HOST_KEY` GitHub Actions secret.

## Reading logs

```bash
# All services, follow:
sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml logs -f --tail=200
# Single service:
sudo -u crm-deploy docker compose -f /opt/crm/stg/compose.stg.yml logs -f --tail=500 app
# What is currently deployed:
sudo -u crm-deploy grep '^APP_IMAGE=' /opt/crm/stg/.env.stg
sudo -u crm-deploy cat /opt/crm/stg/.last-image  # what was running before this deploy
```

## Manual rollback (smoke check went red)

The deploy wrapper records the previous `APP_IMAGE` in `/opt/crm/stg/.last-image`
just before swapping. To revert:

```bash
prev="$(cat /opt/crm/stg/.last-image)"
sudo -u crm-deploy /opt/crm/stg/bin/deploy.sh deploy "${prev}"
curl -fsS https://acme.crm.<stg-domain>/health
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
