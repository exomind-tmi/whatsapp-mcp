#!/usr/bin/env bash
# Builds whatsapp-mcp into dist/: dist/windows-amd64/whatsapp-mcp.exe and
# dist/linux-amd64/whatsapp-mcp, or only the OSes given (windows, linux).
#
# Version: $VERSION when set (CI passes the tag, e.g. v0.3.0); otherwise a dev
# build v<next patch after the latest v* tag>-dev.<unix time>, so every rebuild
# is semver-newer than the previous one and older than the next release.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v go >/dev/null || PATH="/c/Program Files/Go/bin:$PATH" # Git Bash on Windows

if [[ -z "${VERSION:-}" ]]; then
  last=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || echo v0.0.0)
  IFS=. read -r major minor patch <<<"${last#v}"
  patch=${patch%%-*}
  VERSION="v${major}.${minor}.$((patch + 1))-dev.$(date +%s)"
fi
if [[ ! $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "build.sh: VERSION '$VERSION' is not semver (vMAJOR.MINOR.PATCH[-pre])" >&2
  exit 1
fi

build() {
  CGO_ENABLED=0 GOOS=$1 GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "$2" ./cmd/whatsapp-mcp
}
[[ $# -gt 0 ]] || set -- windows linux
for os in "$@"; do
  case $os in
    windows) build windows dist/windows-amd64/whatsapp-mcp.exe ;;
    linux) build linux dist/linux-amd64/whatsapp-mcp ;;
    *) echo "build.sh: unknown OS '$os' (windows, linux)" >&2; exit 1 ;;
  esac
done
echo "$VERSION"
