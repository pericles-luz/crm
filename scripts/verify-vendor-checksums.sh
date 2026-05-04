#!/usr/bin/env bash
# verify-vendor-checksums.sh — SIN-62284
#
# Reads web/static/vendor/CHECKSUMS.txt and recomputes every sha384 hash.
# Exits non-zero on any mismatch, missing file, or unexpected file in the
# vendor tree (defence-in-depth: catches both tampering and forgotten
# CHECKSUMS.txt updates after vendoring a new asset).
#
# Format of CHECKSUMS.txt — one entry per line:
#   sha384-<base64-hash><SPACES><relative-path-from-vendor-root>
# Lines that are blank or start with '#' are ignored.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendor_root="${repo_root}/web/static/vendor"
checksums_file="${vendor_root}/CHECKSUMS.txt"

if [[ ! -f "${checksums_file}" ]]; then
  echo "verify-vendor-checksums: missing ${checksums_file}" >&2
  exit 1
fi

declare -A expected=()
while IFS= read -r line || [[ -n "${line}" ]]; do
  # Strip comments and blanks.
  case "${line}" in
    ''|\#*) continue ;;
  esac
  hash="${line%% *}"
  rest="${line#* }"
  # Trim leading whitespace from the path.
  path="${rest#"${rest%%[![:space:]]*}"}"
  if [[ -z "${hash}" || -z "${path}" ]]; then
    echo "verify-vendor-checksums: malformed entry: ${line}" >&2
    exit 1
  fi
  if [[ "${hash}" != sha384-* ]]; then
    echo "verify-vendor-checksums: only sha384-<base64> hashes are supported (got '${hash}')" >&2
    exit 1
  fi
  expected["${path}"]="${hash}"
done < "${checksums_file}"

if [[ ${#expected[@]} -eq 0 ]]; then
  echo "verify-vendor-checksums: ${checksums_file} has no entries" >&2
  exit 1
fi

status=0

# 1. Every entry in CHECKSUMS.txt must exist and match.
for path in "${!expected[@]}"; do
  abs="${vendor_root}/${path}"
  if [[ ! -f "${abs}" ]]; then
    echo "verify-vendor-checksums: missing file: web/static/vendor/${path}" >&2
    status=1
    continue
  fi
  actual="sha384-$(openssl dgst -sha384 -binary "${abs}" | openssl base64 -A)"
  if [[ "${actual}" != "${expected[${path}]}" ]]; then
    echo "verify-vendor-checksums: hash mismatch for web/static/vendor/${path}" >&2
    echo "  expected: ${expected[${path}]}" >&2
    echo "  actual:   ${actual}" >&2
    status=1
  fi
done

# 2. Every .js under the vendor tree must be listed in CHECKSUMS.txt
#    (catches "I dropped a new vendor file but forgot to update CHECKSUMS.txt").
while IFS= read -r -d '' abs; do
  rel="${abs#"${vendor_root}/"}"
  if [[ -z "${expected[${rel}]+x}" ]]; then
    echo "verify-vendor-checksums: file not listed in CHECKSUMS.txt: web/static/vendor/${rel}" >&2
    status=1
  fi
done < <(find "${vendor_root}" -type f -name '*.js' -print0)

if [[ ${status} -eq 0 ]]; then
  echo "verify-vendor-checksums: ok (${#expected[@]} file(s))"
fi

exit "${status}"
