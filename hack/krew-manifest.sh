#!/usr/bin/env bash
# Fill deploy/krew/palette.yaml from a goreleaser checksums file and print the manifest.
#
#   hack/krew-manifest.sh <version> <base-url> [checksums-file]
#
# version        1.2.3 or v1.2.3 (the goreleaser archive names carry no "v")
# base-url       where the archives are downloaded from, with no trailing slash, for example
#                https://github.com/craig-ai-tooling/kubectl-palette/releases/download/v1.2.3
# checksums-file defaults to dist/checksums.txt
#
# The manifest is never edited by hand: every URI and sha256 comes from this script.
set -euo pipefail

if [ $# -lt 2 ] || [ $# -gt 3 ]; then
  echo "usage: $0 <version> <base-url> [checksums-file]" >&2
  exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ver=${1#v}
base=${2%/}
sums=${3:-$root/dist/checksums.txt}
template=$root/deploy/krew/palette.yaml

[ -f "$sums" ] || { echo "krew-manifest: no checksums file at $sums (run goreleaser first)" >&2; exit 1; }
[ -f "$template" ] || { echo "krew-manifest: missing $template" >&2; exit 1; }

sed_args=(-e "s|@VERSION@|v${ver}|")
for plat in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64 windows_amd64 windows_arm64; do
  ext=tar.gz
  case $plat in windows_*) ext=zip ;; esac
  file="kubectl-palette_${ver}_${plat}.${ext}"
  sum=$(awk -v f="$file" '$2 == f { print $1 }' "$sums")
  if ! [[ $sum =~ ^[0-9a-f]{64}$ ]]; then
    echo "krew-manifest: no sha256 for $file in $sums (wrong version?)" >&2
    exit 1
  fi
  sed_args+=(-e "s|@URI_${plat}@|${base}/${file}|" -e "s|@SHA256_${plat}@|${sum}|")
done

# Drop the template's "fill me in" header; the output is a finished manifest.
sed "${sed_args[@]}" "$template" | sed -e '1,/^apiVersion:/{/^apiVersion:/!d;}'
