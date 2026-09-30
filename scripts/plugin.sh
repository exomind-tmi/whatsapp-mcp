#!/usr/bin/env bash
# Packs the plugin as it ships: text only, no binaries. The launcher in it
# downloads the binary pinned in release.json from GitHub Releases.
#   dist/whatsapp-mcp-plugin.zip  - for Claude Desktop (Customize → Plugins → upload)
#   dist/marketplace/             - local marketplace for Claude Code:
#       claude plugin marketplace add ./dist/marketplace
#       claude plugin install whatsapp@exomind-local
# RELEASE_JSON=<file> packs that file as release.json instead of
# plugin/release.json; scripts/dev-install.sh uses it to pin a local build.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v go >/dev/null || PATH="/c/Program Files/Go/bin:$PATH" # Git Bash on Windows

release=${RELEASE_JSON:-plugin/release.json}
version=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$release" | head -n 1)
if [[ -z $version ]]; then
  echo "plugin.sh: no version in $release" >&2
  exit 1
fi
launcher=scripts/launch-whatsapp-mcp

market=dist/marketplace
stage=$market/plugins/whatsapp
rm -rf "$market"
mkdir -p "$market/.claude-plugin" "$market/plugins"
cp -r plugin "$stage"
cp "$release" "$stage/release.json"
cp LICENSE THIRD_PARTY_NOTICES.md "$stage/"
chmod 0755 "$stage/$launcher"
# The manifest carries the pinned version, so hosts see an update.
sed -i "s/\"version\": \"[^\"]*\"/\"version\": \"${version#v}\"/" "$stage/.claude-plugin/plugin.json"

# grep -I skips binary files, so -L lists them (and empty files).
binaries=$(grep -rIL . "$stage" || true)
if [[ -n $binaries ]]; then
  echo "plugin.sh: the plugin must be text only, found:" >&2
  echo "$binaries" >&2
  exit 1
fi

cat >"$market/.claude-plugin/marketplace.json" <<'EOF'
{
  "name": "exomind-local",
  "description": "Local build of whatsapp-mcp for testing",
  "owner": { "name": "Anton Hokkanen" },
  "plugins": [{ "name": "whatsapp", "source": "./plugins/whatsapp" }]
}
EOF

# Entries without "./" (Desktop rejects them otherwise) and with the exec bit
# on the Linux launcher; mkzip needs no zip tool, so this works in Git Bash.
rm -f dist/whatsapp-mcp-plugin.zip
go run ./scripts/mkzip -x "$launcher" dist/whatsapp-mcp-plugin.zip "$stage"
echo "dist/whatsapp-mcp-plugin.zip ($version)"
