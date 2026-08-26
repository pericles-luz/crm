#!/usr/bin/env bash
# check-compose-unbound-parity.sh — SIN-62332 / ADR 0079 §2 deploy gate.
#
# Asserts: any compose*.yml whose active Caddyfile turns on the
# `on_demand_tls` custom-domain catch-all MUST also bring up the Unbound
# sidecar AND pin Caddy's container DNS resolver to its static ipv4_address
# (`dns: ["<unbound-ip>"]`).
#
# SIN-66592 — the pin MUST be an IP literal that equals the unbound
# service's `ipv4_address`, NOT the service name `"unbound"`. Docker writes
# `dns:` verbatim as `nameserver <value>` into the container's
# /etc/resolv.conf, which requires an IP literal; `"unbound"` is
# unparseable and crashes the container recreate. This gate now rejects the
# old service-name form and enforces that the pin points at the sidecar.
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

# Image whose parser is authoritative for infra/caddy/unbound.conf. Keep in
# sync with the `unbound` service in deploy/compose/compose.stg.yml — a
# config that parses under a different Unbound build proves nothing about
# the one that actually boots on the host.
UNBOUND_IMAGE="${UNBOUND_IMAGE:-mvance/unbound:1.22.0@sha256:76906da36d1806f3387338f15dcf8b357c51ce6897fb6450d6ce010460927e90}"

# unbound_conf_parses <conf-file>
#
# Runs the real `unbound-checkconf` over the sidecar config.
#
# Why this gate exists: this lint used to check only compose<->unbound
# *wiring*, never whether the config it points at is loadable. A config
# using BIND's `deny-answer-address:` (not an Unbound keyword) shipped to
# stg, unbound refused to start, and because Caddy pins `dns:` at the
# sidecar it lost ALL name resolution — every ACME renewal failed with
# "server misbehaving" and a tenant cert expired before anyone noticed.
# An unparseable config is a silent TLS outage on a ~60-day fuse, so it
# must fail here rather than on the host.
unbound_conf_parses() {
	local conf="$1"

	if command -v unbound-checkconf >/dev/null 2>&1; then
		unbound-checkconf "$conf"
		return
	fi

	if command -v docker >/dev/null 2>&1; then
		docker run --rm \
			-v "$(realpath "$conf"):/unbound.conf:ro" \
			--entrypoint unbound-checkconf \
			"$UNBOUND_IMAGE" /unbound.conf
		return
	fi

	log "  neither unbound-checkconf nor docker available — cannot validate"
	return 1
}

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

# unbound_static_ip <compose-file>
#
# Returns the `ipv4_address` assigned to the top-level `unbound:` service
# (long-form `networks: { <net>: { ipv4_address: X } }`). Empty when the
# service has no static IP. SIN-66592.
unbound_static_ip() {
	local f="$1"
	awk '
		/^[[:space:]]{2}unbound:[[:space:]]*$/ { in_u=1; next }
		in_u && /^[[:space:]]{2}[^[:space:]]/ { in_u=0 }
		in_u && /ipv4_address:/ {
			line=$0
			sub(/^.*ipv4_address:[[:space:]]*/, "", line)
			gsub(/[[:space:]"'\'']/, "", line)
			print line
			exit
		}
	' "$f"
}

# caddy_dns_value <compose-file>
#
# Returns the value of the caddy service `dns:` key with brackets, quotes
# and spaces stripped (e.g. `172.29.0.53`). Empty when absent. SIN-66592.
caddy_dns_value() {
	local f="$1"
	awk '
		/^[[:space:]]+caddy:[[:space:]]*$/ { in_caddy=1; next }
		in_caddy && /^[[:space:]][^[:space:]]/ { in_caddy=0 }
		in_caddy && /^[[:space:]]+dns:/ {
			line=$0
			sub(/^[[:space:]]+dns:[[:space:]]*/, "", line)
			gsub(/[][ "'\'']/, "", line)
			print line
			exit
		}
	' "$f"
}

# caddy_dns_pinned_to_unbound <compose-file>
#
# SIN-66592 — the caddy `dns:` pin must be an IPv4 literal that equals the
# unbound sidecar's static ipv4_address. This rejects both the legacy
# service-name form (`dns: ["unbound"]`, which crashes the container
# recreate) and any IP that does not actually point at our resolver.
caddy_dns_pinned_to_unbound() {
	local f="$1"
	local ip dns
	ip=$(unbound_static_ip "$f")
	dns=$(caddy_dns_value "$f")
	[[ -n "$dns" ]] || { log "  dns: pin missing"; return 1; }
	if [[ ! "$dns" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
		log "  dns: pin '${dns}' is not an IPv4 literal (service names crash docker resolv.conf)"
		return 1
	fi
	[[ -n "$ip" ]] || { log "  unbound service has no ipv4_address to pin to"; return 1; }
	if [[ "$dns" != "$ip" ]]; then
		log "  dns: pin '${dns}' != unbound ipv4_address '${ip}'"
		return 1
	fi
	return 0
}

fail=0
files=( "$@" )
default_scan=0
if [[ ${#files[@]} -eq 0 ]]; then
	default_scan=1
	shopt -s nullglob
	files=( deploy/compose/compose*.yml deploy/compose/compose*.yaml )
	if [[ ${#files[@]} -eq 0 ]]; then
		log "no compose files found in deploy/compose/"
		exit 2
	fi
fi

# Only in default-scan mode: the fixture self-test drives this script with
# explicit synthetic compose paths and has no bearing on the real sidecar
# config, so validating it there would just pay the docker cost per case.
if (( default_scan )); then
	unbound_conf="infra/caddy/unbound.conf"
	if [[ -f "$unbound_conf" ]]; then
		log "${unbound_conf}: validating with unbound-checkconf"
		if unbound_conf_parses "$unbound_conf"; then
			log "${unbound_conf}: OK (parses)"
		else
			log "${unbound_conf}: FAIL — config does not parse; the sidecar would crash-loop and take Caddy's DNS (and ACME renewal) down with it"
			fail=1
		fi
	else
		log "${unbound_conf}: FAIL — sidecar config missing"
		fail=1
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
		log "${compose_file}: FAIL — caddy.dns must be unbound's static ipv4_address (IP literal, not the service name)"
		this_fail=1
	fi

	if (( this_fail == 0 )); then
		log "${compose_file}: OK (unbound service + dns pin present)"
	else
		fail=1
	fi
done

exit "$fail"
