#!/usr/bin/env bash
# Hook of the kit's release workflow: runs after the binaries are staged in
# dist/release/, before SHA256SUMS. whatsapp-mcp has notices of its own, so the
# kit's (notices: false) are replaced by this: THIRD_PARTY_NOTICES.md must be
# current, and the license texts, with the source of the GPL and MPL
# components, ship as whatsapp-mcp_<tag>_licenses.zip. $TAG is the release tag;
# the kit is checked out in .mcp-plugin-kit (MCP_PLUGIN_KIT overrides).
set -euo pipefail
cd "$(dirname "$0")/.."
command -v go >/dev/null || PATH="/c/Program Files/Go/bin:$PATH" # Git Bash on Windows

root=$(pwd)
kit=${MCP_PLUGIN_KIT:-$root/.mcp-plugin-kit}

scripts/notices.sh
git diff --exit-code THIRD_PARTY_NOTICES.md
cp LICENSE THIRD_PARTY_NOTICES.md dist/licenses/
(cd "$kit" && go run ./cmd/mkzip "$root/dist/release/whatsapp-mcp_${TAG}_licenses.zip" "$root/dist/licenses")
