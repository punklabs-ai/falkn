#!/bin/sh
set -eu

[ "$#" -eq 2 ] || {
  printf 'usage: package-release.sh <version> <output-directory>\n' >&2
  exit 64
}

version="${1#v}"
output_directory="$2"
script_directory="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
repository_root="$(dirname "$script_directory")"
module_directory="$repository_root"

case "$version" in
  ''|*[!0-9A-Za-z.-]*)
    printf 'invalid release version: %s\n' "$version" >&2
    exit 64
    ;;
esac

mkdir -p "$output_directory"
output_directory="$(CDPATH= cd -- "$output_directory" && pwd)"
temporary_directory="$(mktemp -d "${TMPDIR:-/tmp}/falkn-release.XXXXXX")"
trap 'rm -r "$temporary_directory"' EXIT HUP INT TERM

for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  operating_system="${target%/*}"
  architecture="${target#*/}"
  package_directory="${temporary_directory}/${operating_system}_${architecture}"
  mkdir "$package_directory"

  printf 'building falkn %s for %s/%s\n' "$version" "$operating_system" "$architecture"
  for command in falkn falknd; do
    (
      cd "$module_directory"
      CGO_ENABLED=0 GOOS="$operating_system" GOARCH="$architecture" go build \
        -trimpath \
        -ldflags "-s -w -X github.com/punklabs-ai/falkn/internal/buildinfo.Version=${version}" \
        -o "${package_directory}/${command}" \
        "./cmd/${command}"
    )
  done

  tar -czf "${output_directory}/falkn_${operating_system}_${architecture}.tar.gz" \
    -C "$package_directory" falkn falknd
done

(
  cd "$output_directory"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum falkn_*.tar.gz > checksums.txt
  else
    shasum -a 256 falkn_*.tar.gz > checksums.txt
  fi
)
