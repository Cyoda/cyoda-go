#!/usr/bin/env bash
# Verifies every go.mod in this repo (root + plugins/*) pins
# github.com/cyoda-platform/cyoda-go-spi to the same version.
#
# Exits 0 if all pins agree.
# Exits 1 if any go.mod disagrees, with a readable diff.
#
# Used as a CI gate to prevent the v0.6.1-vs-v0.7.0 drift recurring.
# Compatible with bash 3.2+ (macOS system bash).

set -euo pipefail

# Collect all go.mod files (up to 4 levels deep, skip hidden and vendor dirs).
# Store as newline-separated in a variable for bash 3.2 compatibility
# (mapfile / readarray require bash 4+).
mod_files=$(find . -mindepth 1 -maxdepth 4 -name go.mod -not -path '*/.*' -not -path '*/vendor/*' | sort)

# Extract SPI versions from each go.mod and build a unique-version list.
# Output format: "<version> <path>" for each file that mentions cyoda-go-spi.
all_versions=""
file_version_pairs=""

while IFS= read -r mod; do
  [[ -z "$mod" ]] && continue
  ver=$(awk '/[[:space:]]github\.com\/cyoda-platform\/cyoda-go-spi v/ { print $NF; exit }' "$mod" 2>/dev/null || true)
  if [[ -n "${ver:-}" ]]; then
    all_versions="${all_versions}${ver}"$'\n'
    file_version_pairs="${file_version_pairs}${ver} ${mod}"$'\n'
  fi
done <<< "$mod_files"

# Count distinct versions seen.
distinct=$(printf '%s' "$all_versions" | sort -u | grep -c . || true)

if (( distinct == 0 )); then
  echo "check-spi-pin-sync: no go.mod files reference cyoda-go-spi (nothing to check)"
  exit 0
fi

if (( distinct == 1 )); then
  the_version=$(printf '%s' "$all_versions" | sort -u | head -1)
  echo "check-spi-pin-sync: OK — all manifests pin cyoda-go-spi $the_version"
  exit 0
fi

echo "check-spi-pin-sync: FAIL — cyoda-go-spi pin drift detected"
echo

# Print each version and the files pinned to it.
unique_versions=$(printf '%s' "$all_versions" | sort -u)
while IFS= read -r ver; do
  [[ -z "$ver" ]] && continue
  echo "  $ver:"
  while IFS= read -r pair; do
    [[ -z "$pair" ]] && continue
    pair_ver="${pair%% *}"
    pair_file="${pair#* }"
    if [[ "$pair_ver" == "$ver" ]]; then
      echo "    $pair_file"
    fi
  done <<< "$file_version_pairs"
done <<< "$unique_versions"

echo
echo "Resolution: bump cyoda-go-spi to a single version across root and plugins/*."
exit 1
