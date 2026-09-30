#!/usr/bin/env bash
# Installs a dev build on this machine, for testing the plugin without a release:
#   1. builds whatsapp-mcp for this OS (scripts/build.sh);
#   2. puts it into <home>/bin/<version>/, where the launcher of a plugin
#      pinned to this version finds it;
#   3. packs the plugin (scripts/plugin.sh) with a release.json that pins this
#      version: dist/whatsapp-mcp-plugin.zip and dist/marketplace/.
# plugin/release.json stays untouched. <home> is $WHATSAPP_MCP_HOME or
# ~/.mcp/exomind-tmi/whatsapp-mcp, as for the launcher.
# A running daemon keeps serving until a newer shim replaces it: install the
# packed plugin and start a new session, whose shim (this build) takes over on
# its first call. Or run `whatsapp-mcp stop`: the next call of any session
# starts the newest binary in bin/.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v go >/dev/null || PATH="/c/Program Files/Go/bin:$PATH" # Git Bash on Windows

os=$(go env GOOS)
case $os in
  windows) exe=whatsapp-mcp.exe ;;
  linux) exe=whatsapp-mcp ;;
  *) echo "dev-install.sh: no launcher for $os" >&2; exit 1 ;;
esac
version=$(scripts/build.sh "$os")
built=dist/$os-amd64/$exe

# Like the launcher and the Go code: on Windows the home is USERPROFILE, which
# Git Bash's HOME need not be.
base=$HOME
[[ $os != windows || -z ${USERPROFILE:-} ]] || base=$(cygpath -u "$USERPROFILE")
home=${WHATSAPP_MCP_HOME:-$base/.mcp/exomind-tmi/whatsapp-mcp}
dst=$home/bin/$version
mkdir -p "$dst"
cp "$built" "$dst/.$exe.tmp"
mv -f "$dst/.$exe.tmp" "$dst/$exe"

# Named like a release asset and served from disk: if bin/ loses the build,
# the launcher downloads it again from here.
rel=dist/dev-release
rm -rf "$rel"
mkdir -p "$rel"
asset=$rel/whatsapp-mcp_${version}_${os}_amd64${exe#whatsapp-mcp}
cp "$built" "$asset"
root=$(pwd -W 2>/dev/null || pwd) # Git Bash: C:/..., elsewhere /...
RELEASE_URL_BASE="file:///${root#/}/$rel" \
  scripts/release-json.sh "$version" "$os-amd64=$asset" >dist/dev-release.json

RELEASE_JSON=dist/dev-release.json scripts/plugin.sh >/dev/null
echo "installed $dst/$exe"
echo "packed dist/whatsapp-mcp-plugin.zip and dist/marketplace/ pinned to $version"
