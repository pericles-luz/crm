#!/usr/bin/env bash
# check-compose-unbound-parity.sh — SIN-62332 / ADR 0079 §2 deploy gate.
#
# Asserts: any compose*.yml whose active Caddyfile turns on the
# `on_demand_tls` custom-domain catch-all MUST also bring up the Unbound
# sidecar AND pin Caddy's container DNS resolver to it (`dns: ["unbound"]`).
#
# Why: without the sidecar, Caddy resolves HTTP-01 challenge names through
# the host's /etc/resolv.conf and an attacker-controlled authoritative
# answer (private/loopback IP) re-opens the F44 DNS-rebinding attack on
# ACME issuance — see ADR 0079 §2 and re-review SIN-62328 § R-A.
#
# Usage:
#   scripts/check-compose-unbound-parity.sh                            # default scan
#   scripts/check-compose-unbound-parity.sh path/to/compose.yml [...]  # explicit
#
# Default scan is `deploy/compose/compose*.yml`.
#
# Exit codes:
#   0  every compose passes parity (or has no on_demand_tls catch-all)
#   1  at least one compose violates parity
#   2  usage error / unable to locate inputs

set -euo pipefail

log() { echo "$@" >&2; }

# active_caddyfile <compose-file>
#
# Reads the `caddy` service's `command:` list and pulls the path passed
# after `--config`. If the command does not override --config, Caddy's
# default of /etc/caddy/Caddyfile applies. Returns the basename only.
active_caddyfile() {
	local file="$1"
	awk '
		/^[[:space:]]+caddy:[[:space:]]*$/ { in_caddy=1; next }
		in_caddy && /^[[:space:]][^[:space:]]/ { in_caddy=0 }
		in_caddy && /command:/ {
			line=$0
			# strip everything before the first /etc/caddy/ token
			if (match(line, /\/etc\/caddy\/[A-Za-z0-9._-]+/)) {
				name=substr(line, RSTART+11, RLENGTH-11)
				print name
				exit
			}
		}
	' "$file"
}

# caddy_etc_mount <compose-file>
#
# Returns the host-side path that the caddy service mounts at
# /etc/caddy:ro (e.g. ../caddy or ./caddy). Empty string when missing.
caddy_etc_mount() {
	local file="$1"
	awk '
		/^[[:space:]]+caddy:[[:space:]]*$/ { in_caddy=1; next }
		in_caddy && /^[[:space:]][^[:space:]]/ { in_caddy=0 }
		in_caddy && /^[[:space:]]+volumes:/ { in_vol=1; next }
		in_caddy && in_vol && /^[[:space:]]+[a-zA-Z]/ { in_vol=0 }
		in_caddy && in_vol {
			# match  - <host-path>:/etc/caddy[:ro]
			if (match($0, /[^[:space:]"-][^:]*:\/etc\/caddy(:ro)?[[:space:]]*$/)) {
				token=substr($0, RSTART, RLENGTH)
				sub(/:\/etc\/caddy.*/, "", token)
				gsub(/^[[:space:]"-]+/, "", token)
				print token
				exit
			}
		}
	' "$file"
}

# caddyfile_has_on_demand <path>
#
# True iff the file contains an uncommented `on_demand_tls` directive
# (either the global block opener `on_demand_tls {` or the inline form).
caddyfile_has_on_demand() {
	local f="$1"
	[[ -r "$f" ]] || return 1
	# strip line comments first so a `# on_demand_tls` reference does not
	# false-positive; then look for a directive token at start-of-line.
	sed 's/#.*$//' "$f" | grep -E '^[[:space:]]*on_demand_tls([[:space:]]|\{|$)' >/dev/null
}

# compose_has_unbound_service <compose-file>
compose_has_unbound_service() {
	local f="$1"
	# top-level service entry sits at column 2 (under `services:`)
	grep -E '^[[:space:]]{2}unbound:[[:space:]]*$' "$f" >/dev/null
}

# caddy_dns_pinned_to_unbound <compose-file>
caddy_dns_pinned_to_unbound() {
	local f="$1"
	awk '
		/^[[:space:]]+caddy:[[:space:]]*$/ { in_caddy=1; next }
		in_caddy && /^[[:space:]][^[:space:]]/ { in_caddy=0 }
		in_caddy && /^[[:space:]]+dns:/ {
			line=$0
			sub(/^[[:space:]]+dns:[[:space:]]*/, "", line)
			# Accept any of: ["unbound"], [unbound], "unbound", unbound (single-line forms)
			if (line ~ /[\["'\'']?unbound[\]"'\'']?/) print "ok"
			exit
		}
	' "$f" | grep -q ok
}

fail=0
files=( "$@" )
if [[ ${#files[@]} -eq 0 ]]; then
	shopt -s nullglob
	files=( deploy/compose/compose*.yml deploy/compose/compose*.yaml )
	if [[ ${#files[@]} -eq 0 ]]; then
		log "no compose files found in deploy/compose/"
		exit 2
	fi
fi

for compose_file in "${files[@]}"; do
	if [[ ! -f "$compose_file" ]]; then
		log "skip: ${compose_file} (not a file)"
		continue
	fi

	active=$(active_caddyfile "$compose_file")
	if [[ -z "$active" ]]; then
		active="Caddyfile"
	fi

	mount_rel=$(caddy_etc_mount "$compose_file")
	if [[ -z "$mount_rel" ]]; then
		log "${compose_file}: no caddy /etc/caddy mount found — skipping"
		continue
	fi

	compose_dir=$(dirname "$compose_file")
	caddyfile_path="${compose_dir}/${mount_rel}/${active}"
	caddyfile_path=$(realpath -m "$caddyfile_path")

	# Stg compose mounts `./caddy:/etc/caddy:ro` because the operator
	# assembles `/opt/crm/stg/caddy/` on the VPS — that path does not
	# exist in the source tree. Fall back to the canonical source-tree
	# location so the lint can still resolve the active Caddyfile.
	if [[ ! -f "$caddyfile_path" ]]; then
		fallback="${compose_dir}/../caddy/${active}"
		fallback=$(realpath -m "$fallback")
		if [[ -f "$fallback" ]]; then
			caddyfile_path="$fallback"
		else
			log "${compose_file}: active Caddyfile ${caddyfile_path} not found — skipping"
			continue
		fi
	fi

	if ! caddyfile_has_on_demand "$caddyfile_path"; then
		log "${compose_file}: ${caddyfile_path} has no on_demand_tls — parity not required"
		continue
	fi

	log "${compose_file}: on_demand_tls active in ${caddyfile_path} — Unbound parity required"

	this_fail=0
	if ! compose_has_unbound_service "$compose_file"; then
		log "${compose_file}: FAIL — missing top-level 'unbound:' service"
		this_fail=1
	fi
	if ! caddy_dns_pinned_to_unbound "$compose_file"; then
		log "${compose_file}: FAIL — caddy.dns must include 'unbound'"
		this_fail=1
	fi

	if (( this_fail == 0 )); then
		log "${compose_file}: OK (unbound service + dns pin present)"
	else
		fail=1
	fi
done

exit "$fail"
