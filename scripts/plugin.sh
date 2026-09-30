#!/usr/bin/env bash
# Builds and packs the plugin:
#   dist/whatsapp-mcp-plugin.zip  - for Claude Desktop (Customize → Plugins → upload)
#   dist/marketplace/             - local marketplace for Claude Code, e.g. on Ubuntu:
#       claude plugin marketplace add ./dist/marketplace
#       claude plugin install whatsapp@exomind-local
# Set SKIP_BUILD=1 to pack the binaries already in dist/.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v go >/dev/null || PATH="/c/Program Files/Go/bin:$PATH" # Git Bash on Windows

[[ "${SKIP_BUILD:-}" == 1 ]] || scripts/build.sh >/dev/null
scripts/notices.sh >/dev/null
version=$(cat dist/VERSION)

market=dist/marketplace
stage=$market/plugins/whatsapp
rm -rf "$market"
mkdir -p "$market/.claude-plugin" "$stage/.claude-plugin" "$stage/server"

cp plugin/.mcp.json LICENSE THIRD_PARTY_NOTICES.md "$stage/"
cp -r dist/licenses "$stage/licenses"
sed "s/\"version\": \"0.0.0\"/\"version\": \"${version#v}\"/" plugin/.claude-plugin/plugin.json \
  >"$stage/.claude-plugin/plugin.json"
# Linux binary first: MSYS cp treats "whatsapp-mcp" as "whatsapp-mcp.exe"
# when the latter already exists and would overwrite it.
cp dist/linux-amd64/whatsapp-mcp "$stage/server/whatsapp-mcp"
cp dist/windows-amd64/whatsapp-mcp.exe "$stage/server/whatsapp-mcp.exe"
chmod 0755 "$stage/server/whatsapp-mcp"

cat >"$market/.claude-plugin/marketplace.json" <<'EOF'
{
  "name": "exomind-local",
  "description": "Local build of whatsapp-mcp for testing",
  "owner": { "name": "Anton Hokkanen" },
  "plugins": [{ "name": "whatsapp", "source": "./plugins/whatsapp" }]
}
EOF

# Entries without "./" (Desktop rejects them otherwise) and with the exec bit
# on the Linux binary; mkzip needs no zip tool, so this works in Git Bash.
rm -f dist/whatsapp-mcp-plugin.zip
go run ./scripts/mkzip -x server/whatsapp-mcp dist/whatsapp-mcp-plugin.zip "$stage"
echo "dist/whatsapp-mcp-plugin.zip ($version)"
