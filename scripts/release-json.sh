#!/usr/bin/env bash
# Prints plugin/release.json for a version: per platform, where the launcher
# downloads the binary and its SHA-256.
#
#   scripts/release-json.sh v0.3.0 \
#     windows-amd64=dist/release/whatsapp-mcp_v0.3.0_windows_amd64.exe \
#     linux-amd64=dist/release/whatsapp-mcp_v0.3.0_linux_amd64
#
# URLs point to the GitHub Release of that version, named after each file;
# RELEASE_URL_BASE replaces the base (scripts/dev-install.sh uses file://).
# The Linux launcher parses this shape with sed: keep assets flat objects of
# strings without quotes or braces in the values.
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: release-json.sh <version> <platform>=<file>..." >&2
  exit 2
fi
version=$1
shift
if [[ ! $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "release-json.sh: version '$version' is not semver" >&2
  exit 1
fi
base=${RELEASE_URL_BASE:-https://github.com/exomind-tmi/whatsapp-mcp/releases/download/$version}

# Collect first, print last: a bad argument prints nothing.
assets=
for arg in "$@"; do
  platform=${arg%%=*} file=${arg#*=}
  if [[ ! $platform =~ ^[a-z0-9]+-[a-z0-9]+$ || ! -s $file ]]; then
    echo "release-json.sh: '$arg': expected <os>-<arch>=<non-empty file>" >&2
    exit 1
  fi
  sum=$(sha256sum "$file")
  assets+=$(printf '%s\n    "%s": {\n      "url": "%s/%s",\n      "sha256": "%s"\n    }' \
    "${assets:+,}" "$platform" "$base" "$(basename "$file")" "${sum%% *}")
done
printf '{\n  "version": "%s",\n  "assets": {%s\n  }\n}\n' "$version" "$assets"
