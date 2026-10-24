#!/usr/bin/env bash
# CycloneDX SBOM of the limen binary (kavanara-baseline ADR-0008).
#
#   scripts/sbom.sh [OUT]   write OUT (default limen.cdx.json)
#
# limen is one Go module. What the binary is built from is the Go standard
# library of the toolchain that builds it plus every module dependency, so the
# SBOM lists exactly those. Licences of module dependencies are read and gated
# by the tests (licence_test.go, `make test`); this script only inventories.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$root/limen.cdx.json}"
cd "$root"

version="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
goversion="$(go env GOVERSION)"

components=()
components+=("$(printf '{"type":"library","name":"stdlib","version":"%s","purl":"pkg:golang/stdlib@%s","licenses":[{"license":{"id":"BSD-3-Clause"}}]}' "$goversion" "$goversion")")
while read -r path ver; do
  [[ -n "$path" ]] || continue
  components+=("$(printf '{"type":"library","name":"%s","version":"%s","purl":"pkg:golang/%s@%s"}' "$path" "$ver" "$path" "$ver")")
done < <(go list -m -f '{{if not .Main}}{{.Path}} {{.Version}}{{end}}' all)

{
  printf '{"bomFormat":"CycloneDX","specVersion":"1.6","version":1,'
  printf '"metadata":{"timestamp":"%s","component":{"type":"application","name":"limen","version":"%s","purl":"pkg:golang/github.com/matthiaswegner/limen@%s","licenses":[{"license":{"id":"Apache-2.0"}}]}},' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$version" "$version"
  printf '"components":['
  (IFS=,; printf '%s' "${components[*]}")
  printf ']}\n'
} > "$out"
echo "sbom: wrote $out (${#components[@]} component(s))"
